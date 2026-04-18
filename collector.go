package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const dockerSock = "/var/run/docker.sock"
const dockerAPI  = "http://localhost/v1.41"

type LogLine struct {
	Timestamp time.Time `json:"timestamp"`
	Container string    `json:"container"`
	Message   string    `json:"message"`
}

type Subscriber chan LogLine

type ContainerInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // "running" | "stopped"
}

type Collector struct {
	httpClient  *http.Client
	mu          sync.RWMutex
	subscribers map[string]map[int]Subscriber
	nextID      int
	persister   *Persister
	containers  map[string]ContainerInfo // keyed by name
}

func NewCollector(p *Persister) *Collector {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", dockerSock)
		},
	}
	return &Collector{
		httpClient:  &http.Client{Transport: transport},
		subscribers: make(map[string]map[int]Subscriber),
		persister:   p,
		containers:  make(map[string]ContainerInfo),
	}
}

func (c *Collector) apiGet(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dockerAPI+path, nil)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// ── Container listing ─────────────────────────────────────────────────────────

type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	State string   `json:"State"`
}

func (c *Collector) listRunning(ctx context.Context) ([]dockerContainer, error) {
	resp, err := c.apiGet(ctx, "/containers/json?all=false")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ctrs []dockerContainer
	return ctrs, json.NewDecoder(resp.Body).Decode(&ctrs)
}

// ── TTY detection ─────────────────────────────────────────────────────────────

type dockerInspect struct {
	Config struct {
		Tty bool `json:"Tty"`
	} `json:"Config"`
}

func (c *Collector) isTTY(ctx context.Context, id string) bool {
	resp, err := c.apiGet(ctx, "/containers/"+id+"/json")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var info dockerInspect
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return false
	}
	return info.Config.Tty
}

// ── Subscribe / broadcast ─────────────────────────────────────────────────────

func (c *Collector) Subscribe(name string) (int, Subscriber) {
	ch := make(Subscriber, 256)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	if c.subscribers[name] == nil {
		c.subscribers[name] = make(map[int]Subscriber)
	}
	c.subscribers[name][id] = ch
	return id, ch
}

func (c *Collector) Unsubscribe(name string, id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if subs, ok := c.subscribers[name]; ok {
		delete(subs, id)
	}
}

func (c *Collector) broadcast(line LogLine) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ch := range c.subscribers[line.Container] {
		select {
		case ch <- line:
		default:
		}
	}
}

// ── Start ─────────────────────────────────────────────────────────────────────

func (c *Collector) Start(ctx context.Context) {
	ctrs, err := c.listRunning(ctx)
	if err != nil {
		log.Printf("failed to list containers: %v", err)
		return
	}

	c.mu.Lock()
	for _, ctr := range ctrs {
		name := strings.TrimPrefix(ctr.Names[0], "/")
		c.containers[name] = ContainerInfo{ID: ctr.ID, Name: name, Status: "running"}
	}
	c.mu.Unlock()

	for _, ctr := range ctrs {
		name := strings.TrimPrefix(ctr.Names[0], "/")
		go c.streamContainer(ctx, ctr.ID, name)
	}

	go c.watchEvents(ctx)
}

// ── Log streaming ─────────────────────────────────────────────────────────────

func (c *Collector) streamContainer(ctx context.Context, id, name string) {
	query := url.Values{
		"stdout":     {"1"},
		"stderr":     {"1"},
		"follow":     {"1"},
		"timestamps": {"1"},
		"since":      {fmt.Sprintf("%d", time.Now().Unix()-1)},
	}

	resp, err := c.apiGet(ctx, "/containers/"+id+"/logs?"+query.Encode())
	if err != nil {
		log.Printf("logs error for %s: %v", name, err)
		return
	}
	defer resp.Body.Close()

	hasTTY := c.isTTY(ctx, id)

	var scanner *bufio.Scanner
	if hasTTY {
		scanner = bufio.NewScanner(resp.Body)
	} else {
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(demuxDockerStream(resp.Body, pw))
		}()
		scanner = bufio.NewScanner(pr)
	}

	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		ts, msg := splitTimestamp(scanner.Text())
		line := LogLine{Timestamp: ts, Container: name, Message: msg}
		c.persister.Write(line)
		c.broadcast(line)
	}

	c.mu.Lock()
	if ci, ok := c.containers[name]; ok {
		ci.Status = "stopped"
		c.containers[name] = ci
	}
	c.mu.Unlock()
}

// demuxDockerStream parses Docker's multiplexed log format (8-byte header + payload).
func demuxDockerStream(src io.Reader, dst io.Writer) error {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(src, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if _, err := io.CopyN(dst, src, int64(size)); err != nil {
			return err
		}
	}
}

func splitTimestamp(line string) (time.Time, string) {
	idx := strings.Index(line, " ")
	if idx > 0 {
		ts, err := time.Parse(time.RFC3339Nano, line[:idx])
		if err == nil {
			return ts.UTC(), line[idx+1:]
		}
	}
	return time.Now().UTC(), line
}

// ── Event watching ────────────────────────────────────────────────────────────

type dockerEvent struct {
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

func (c *Collector) watchEvents(ctx context.Context) {
	query := fmt.Sprintf("type=container&filters=%s",
		url.QueryEscape(`{"event":["start","die"]}`))

	for {
		if err := c.consumeEvents(ctx, query); err != nil && ctx.Err() == nil {
			log.Printf("docker events error: %v — retrying in 5s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (c *Collector) consumeEvents(ctx context.Context, query string) error {
	resp, err := c.apiGet(ctx, "/events?"+query)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var evt dockerEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			continue
		}
		name := evt.Actor.Attributes["name"]
		id := evt.Actor.ID
		switch evt.Action {
		case "start":
			c.mu.Lock()
			c.containers[name] = ContainerInfo{ID: id, Name: name, Status: "running"}
			c.mu.Unlock()
			go c.streamContainer(ctx, id, name)
		case "die":
			c.mu.Lock()
			if ci, ok := c.containers[name]; ok {
				ci.Status = "stopped"
				c.containers[name] = ci
			}
			c.mu.Unlock()
		}
	}
	return scanner.Err()
}

// ── Public query ──────────────────────────────────────────────────────────────

func (c *Collector) GetContainers() []ContainerInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]ContainerInfo, 0, len(c.containers))
	for _, info := range c.containers {
		result = append(result, info)
	}
	return result
}
