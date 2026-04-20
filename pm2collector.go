package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

type PM2Collector struct {
	collector *Collector
	sockPath  string
}

func NewPM2Collector(c *Collector, sockPath string) *PM2Collector {
	return &PM2Collector{collector: c, sockPath: sockPath}
}

func (p *PM2Collector) Start(ctx context.Context) {
	go p.run(ctx)
}

func (p *PM2Collector) run(ctx context.Context) {
	for {
		if err := p.connect(ctx); err != nil && ctx.Err() == nil {
			log.Printf("pm2: %v — retrying in 5s", err)
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

func (p *PM2Collector) connect(ctx context.Context) error {
	conn, err := net.Dial("unix", p.sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	r := bufio.NewReader(conn)
	for {
		fields, err := readAMPFrame(r)
		if err != nil {
			return err
		}
		if len(fields) >= 2 {
			p.handleLog(fields)
		}
	}
}

// readAMPFrame parses one AMP frame: [version:1][argc:1] then per-arg [len:4BE][data]
func readAMPFrame(r io.Reader) ([][]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	argc := int(header[1])
	fields := make([][]byte, argc)
	for i := 0; i < argc; i++ {
		var l uint32
		if err := binary.Read(r, binary.BigEndian, &l); err != nil {
			return nil, err
		}
		fields[i] = make([]byte, l)
		if _, err := io.ReadFull(r, fields[i]); err != nil {
			return nil, err
		}
	}
	return fields, nil
}

type pm2Packet struct {
	Process struct {
		Name string `msgpack:"name"`
	} `msgpack:"process"`
	Data string `msgpack:"data"`
}

func (p *PM2Collector) handleLog(fields [][]byte) {
	var event string
	if err := msgpack.Unmarshal(fields[0], &event); err != nil {
		return
	}
	if event != "log:out" && event != "log:err" {
		return
	}

	var packet pm2Packet
	if err := msgpack.Unmarshal(fields[1], &packet); err != nil {
		return
	}
	if packet.Process.Name == "" {
		return
	}

	name := "pm2:" + packet.Process.Name

	p.collector.mu.Lock()
	if _, exists := p.collector.containers[name]; !exists {
		p.collector.containers[name] = ContainerInfo{ID: name, Name: name, Status: "running", Source: "pm2"}
	}
	p.collector.mu.Unlock()

	for _, line := range strings.Split(strings.TrimRight(packet.Data, "\r\n"), "\n") {
		if line == "" {
			continue
		}
		ll := LogLine{Timestamp: time.Now().UTC(), Container: name, Message: line}
		p.collector.persister.Write(ll)
		p.collector.broadcast(ll)
	}
}
