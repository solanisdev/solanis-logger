package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Persister struct {
	dir     string
	writeCh chan LogLine
}

func NewPersister(dir string) *Persister {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("failed to create logs dir: %v", err)
	}
	p := &Persister{
		dir:     dir,
		writeCh: make(chan LogLine, 4096),
	}
	go p.worker()
	return p
}

func (p *Persister) Write(line LogLine) {
	select {
	case p.writeCh <- line:
	default:
	}
}

func (p *Persister) worker() {
	files := make(map[string]*os.File)
	for line := range p.writeCh {
		date := line.Timestamp.UTC().Format("2006-01-02")
		key := line.Container + "/" + date

		f, ok := files[key]
		if !ok {
			dir := filepath.Join(p.dir, line.Container)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				log.Printf("mkdir error: %v", err)
				continue
			}
			var err error
			f, err = os.OpenFile(filepath.Join(dir, date+".txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				log.Printf("open file error: %v", err)
				continue
			}
			files[key] = f
		}

		fmt.Fprintf(f, "%s [%s] %s\n",
			line.Timestamp.UTC().Format(time.RFC3339),
			line.Container,
			line.Message,
		)
	}
	for _, f := range files {
		f.Close()
	}
}

func (p *Persister) Search(containerName, date, query string) ([]string, error) {
	filename := filepath.Join(p.dir, containerName, date+".txt")
	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	lowerQuery := strings.ToLower(query)
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if lowerQuery == "" || strings.Contains(strings.ToLower(line), lowerQuery) {
			lines = append(lines, line)
		}
	}
	if lines == nil {
		return []string{}, nil
	}
	return lines, nil
}

func (p *Persister) ListDates(containerName string) ([]string, error) {
	dir := filepath.Join(p.dir, containerName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var dates []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".txt") {
			dates = append(dates, strings.TrimSuffix(e.Name(), ".txt"))
		}
	}
	sort.Strings(dates)
	return dates, nil
}

func (p *Persister) ListKnownContainers() ([]string, error) {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}
