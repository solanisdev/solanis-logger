package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type PM2Collector struct {
	collector *Collector
	logsDir   string
	known     map[string]bool
}

func NewPM2Collector(c *Collector, logsDir string) *PM2Collector {
	return &PM2Collector{collector: c, logsDir: logsDir, known: make(map[string]bool)}
}

func (p *PM2Collector) Start(ctx context.Context) {
	p.scan(ctx)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.scan(ctx)
			}
		}
	}()
}

func (p *PM2Collector) scan(ctx context.Context) {
	entries, err := os.ReadDir(p.logsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		path := filepath.Join(p.logsDir, e.Name())
		if p.known[path] {
			continue
		}
		p.known[path] = true
		name := pm2ProcessName(e.Name())

		p.collector.mu.Lock()
		if _, exists := p.collector.containers[name]; !exists {
			p.collector.containers[name] = ContainerInfo{ID: name, Name: name, Status: "running", Source: "pm2"}
		}
		p.collector.mu.Unlock()

		go p.tailFile(ctx, path, name)
	}
}

func pm2ProcessName(filename string) string {
	name := strings.TrimSuffix(filename, ".log")
	name = strings.TrimSuffix(name, "-out")
	name = strings.TrimSuffix(name, "-error")
	return "pm2:" + name
}

func (p *PM2Collector) tailFile(ctx context.Context, path, name string) {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("pm2: cannot open %s: %v", path, err)
		return
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return
	}

	reader := bufio.NewReader(f)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var partial string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				line, err := reader.ReadString('\n')
				partial += line
				if err != nil {
					break
				}
				msg := strings.TrimRight(partial, "\r\n")
				partial = ""
				if msg == "" {
					continue
				}
				ll := LogLine{
					Timestamp: time.Now().UTC(),
					Container: name,
					Message:   msg,
				}
				p.collector.persister.Write(ll)
				p.collector.broadcast(ll)
			}
		}
	}
}
