package main

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultMaxLines        = 100_000
	idleCloseAfter         = 10 * time.Minute
	idleSweepInterval      = 60 * time.Second
	retentionSweepInterval = time.Hour
)

type activeFile struct {
	f         *os.File
	count     int
	firstTime time.Time
	lastWrite time.Time
}

type rotateReq struct {
	container string
	date      string
	done      chan struct{}
}

type Persister struct {
	dir            string
	writeCh        chan LogLine
	rotateCh       chan rotateReq
	maxLines       int
	retentionDays  int
	startTime      time.Time
	dropped        uint64 // atomic
}

func NewPersister(dir string) *Persister {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("failed to create logs dir: %v", err)
	}

	maxLines := defaultMaxLines
	if v := os.Getenv("MAX_LOG_LINES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxLines = n
		} else {
			log.Printf("invalid MAX_LOG_LINES=%q, using default %d", v, defaultMaxLines)
		}
	}

	retentionDays := 0
	if v := os.Getenv("LOG_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			retentionDays = n
		} else {
			log.Printf("invalid LOG_RETENTION_DAYS=%q, retention disabled", v)
		}
	}

	p := &Persister{
		dir:           dir,
		writeCh:       make(chan LogLine, 4096),
		rotateCh:      make(chan rotateReq, 8),
		maxLines:      maxLines,
		retentionDays: retentionDays,
		startTime:     time.Now(),
	}
	go p.worker()
	if retentionDays > 0 {
		go p.retentionLoop()
	}
	return p
}

func (p *Persister) Write(line LogLine) {
	select {
	case p.writeCh <- line:
	default:
		atomic.AddUint64(&p.dropped, 1)
	}
}

// DroppedCount returns the number of log lines dropped because the write
// channel was full.
func (p *Persister) DroppedCount() uint64 {
	return atomic.LoadUint64(&p.dropped)
}

// Uptime returns how long the persister has been running.
func (p *Persister) Uptime() time.Duration {
	return time.Since(p.startTime)
}

// RetentionDays returns the configured retention window, or 0 if disabled.
func (p *Persister) RetentionDays() int {
	return p.retentionDays
}

// RotateActive closes the persister's cached handle for a given container/date,
// if any. After this returns, the next incoming line for that key opens a fresh
// handle via the normal code path. Safe to call from any goroutine.
func (p *Persister) RotateActive(container, date string) {
	done := make(chan struct{})
	p.rotateCh <- rotateReq{container: container, date: date, done: done}
	<-done
}

func (p *Persister) worker() {
	files := make(map[string]*activeFile)
	defer func() {
		for _, af := range files {
			af.f.Close()
		}
	}()

	idleTicker := time.NewTicker(idleSweepInterval)
	defer idleTicker.Stop()

	for {
		select {
		case line, ok := <-p.writeCh:
			if !ok {
				return
			}
			p.handleLine(files, line)
		case req := <-p.rotateCh:
			key := req.container + "/" + req.date
			if af, ok := files[key]; ok {
				af.f.Sync()
				af.f.Close()
				delete(files, key)
			}
			close(req.done)
		case now := <-idleTicker.C:
			for key, af := range files {
				if now.Sub(af.lastWrite) > idleCloseAfter {
					af.f.Sync()
					af.f.Close()
					delete(files, key)
				}
			}
		}
	}
}

func (p *Persister) handleLine(files map[string]*activeFile, line LogLine) {
	date := line.Timestamp.UTC().Format("2006-01-02")
	key := line.Container + "/" + date

	af, ok := files[key]
	if !ok {
		dir := filepath.Join(p.dir, line.Container)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("mkdir error: %v", err)
			return
		}
		path := filepath.Join(dir, date+".txt")

		existingCount, existingFirstTime, err := countLinesAndFirstTime(path)
		if err != nil && !os.IsNotExist(err) {
			log.Printf("count existing lines %s: %v", path, err)
		}

		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Printf("open file error: %v", err)
			return
		}

		firstTime := existingFirstTime
		if existingCount == 0 {
			firstTime = line.Timestamp.UTC()
		}
		af = &activeFile{f: f, count: existingCount, firstTime: firstTime}
		files[key] = af
	}

	fmt.Fprintf(af.f, "%s [%s] %s\n",
		line.Timestamp.UTC().Format(time.RFC3339),
		line.Container,
		line.Message,
	)
	af.count++
	af.lastWrite = time.Now()

	if af.count >= p.maxLines {
		endTime := line.Timestamp.UTC()
		af.f.Sync()
		af.f.Close()
		delete(files, key)

		if err := rotateToZip(p.dir, line.Container, date, af.firstTime, endTime); err != nil {
			log.Printf("rotate to zip %s/%s: %v", line.Container, date, err)
			// best-effort: reopen in append mode so writes continue. Reset
			// count to 0 so we don't thrash-retry on every subsequent write;
			// we'll try again once the file grows another maxLines lines.
			path := filepath.Join(p.dir, line.Container, date+".txt")
			f, oerr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if oerr != nil {
				log.Printf("reopen after failed rotation %s: %v", path, oerr)
				return
			}
			files[key] = &activeFile{f: f, count: 0, firstTime: af.firstTime, lastWrite: af.lastWrite}
			return
		}

		path := filepath.Join(p.dir, line.Container, date+".txt")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			log.Printf("reopen after rotation %s: %v", path, err)
			return
		}
		files[key] = &activeFile{f: f, count: 0, firstTime: endTime, lastWrite: af.lastWrite}
	}
}

// retentionLoop deletes .txt and .zip files older than retentionDays,
// based on file modification time.
func (p *Persister) retentionLoop() {
	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()
	p.sweepRetention()
	for range ticker.C {
		p.sweepRetention()
	}
}

func (p *Persister) sweepRetention() {
	if p.retentionDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(p.retentionDays) * 24 * time.Hour)
	err := filepath.WalkDir(p.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".txt") && !strings.HasSuffix(name, ".zip") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				log.Printf("retention: remove %s: %v", path, err)
			} else {
				log.Printf("retention: removed %s (older than %d days)", path, p.retentionDays)
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("retention walk: %v", err)
	}
}

// countLinesAndFirstTime reads an existing log file, returning the total line
// count and the UTC timestamp parsed from the first line's RFC3339 prefix.
// Returns (0, zero, nil) for an empty file and (0, zero, err) for a missing
// file (err satisfies os.IsNotExist).
func countLinesAndFirstTime(path string) (int, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	count := 0
	var firstTime time.Time
	first := true
	for scanner.Scan() {
		if first {
			line := scanner.Text()
			if idx := strings.IndexByte(line, ' '); idx > 0 {
				if t, err := time.Parse(time.RFC3339, line[:idx]); err == nil {
					firstTime = t.UTC()
				}
			}
			first = false
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return count, firstTime, err
	}
	return count, firstTime, nil
}

// rotateToZip archives <dir>/<container>/<date>.txt into a zip named
// <start>-<end>-<DD-MM-YYYY>-<container>.zip in the same container folder,
// where <start> and <end> are "HHhMMmSSs" labels in the server's local
// timezone (TZ env var). Removes the source on success. date is YYYY-MM-DD.
//
// If startTime > endTime (can happen when synthetic/out-of-order timestamps
// land in the file), they are swapped so the filename is still readable.
func rotateToZip(logsDir, container, date string, startTime, endTime time.Time) error {
	containerDir := filepath.Join(logsDir, container)
	srcPath := filepath.Join(containerDir, date+".txt")

	if endTime.Before(startTime) {
		startTime, endTime = endTime, startTime
	}

	ddmmyyyy, err := toDDMMYYYY(date)
	if err != nil {
		return fmt.Errorf("parse date %q: %w", date, err)
	}

	startLabel := formatHMS(startTime)
	endLabel := formatHMS(endTime)
	safeContainer := sanitizeForFilename(container)
	base := fmt.Sprintf("%s-%s-%s-%s", startLabel, endLabel, ddmmyyyy, safeContainer)
	zipPath := filepath.Join(containerDir, base+".zip")
	for i := 2; ; i++ {
		if _, err := os.Stat(zipPath); os.IsNotExist(err) {
			break
		} else if err != nil {
			return fmt.Errorf("stat %s: %w", zipPath, err)
		}
		zipPath = filepath.Join(containerDir, fmt.Sprintf("%s_%d.zip", base, i))
	}

	entryName := fmt.Sprintf("%s_%s-%s.txt", date, startLabel, endLabel)

	zf, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("create zip %s: %w", zipPath, err)
	}
	zw := zip.NewWriter(zf)

	src, err := os.Open(srcPath)
	if err != nil {
		zw.Close()
		zf.Close()
		os.Remove(zipPath)
		return fmt.Errorf("open source %s: %w", srcPath, err)
	}

	ew, err := zw.CreateHeader(&zip.FileHeader{Name: entryName, Method: zip.Deflate})
	if err != nil {
		src.Close()
		zw.Close()
		zf.Close()
		os.Remove(zipPath)
		return fmt.Errorf("zip header: %w", err)
	}
	if _, err := io.Copy(ew, src); err != nil {
		src.Close()
		zw.Close()
		zf.Close()
		os.Remove(zipPath)
		return fmt.Errorf("copy into zip: %w", err)
	}
	src.Close()

	if err := zw.Close(); err != nil {
		zf.Close()
		os.Remove(zipPath)
		return fmt.Errorf("close zip writer: %w", err)
	}
	if err := zf.Close(); err != nil {
		os.Remove(zipPath)
		return fmt.Errorf("close zip file: %w", err)
	}

	if err := os.Remove(srcPath); err != nil {
		return fmt.Errorf("remove source %s: %w", srcPath, err)
	}
	log.Printf("rotated %s → %s", srcPath, zipPath)
	return nil
}

func toDDMMYYYY(isoDate string) (string, error) {
	parts := strings.Split(isoDate, "-")
	if len(parts) != 3 {
		return "", fmt.Errorf("expected YYYY-MM-DD, got %q", isoDate)
	}
	return parts[2] + "-" + parts[1] + "-" + parts[0], nil
}

// formatHMS returns "HHhMMmSSs" for the given time, converted to the server's
// local timezone (TZ env var, default UTC). Using local time here is the
// whole point: operators reading the filename expect their wall clock.
func formatHMS(t time.Time) string {
	l := t.In(time.Local)
	return fmt.Sprintf("%02dh%02dm%02ds", l.Hour(), l.Minute(), l.Second())
}

// sanitizeForFilename replaces filesystem-hostile characters in a container
// name so it can safely appear in the zip filename.
func sanitizeForFilename(name string) string {
	repl := func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', ' ':
			return '_'
		}
		return r
	}
	return strings.Map(repl, name)
}

// SearchOpts controls how Search reads lines from a log file.
// Limit == 0 means unlimited. Tail == true means return the LAST Limit matches;
// tail without a limit returns all matches in file order.
type SearchOpts struct {
	Query string
	Limit int
	Tail  bool
}

func (p *Persister) Search(containerName, date string, opts SearchOpts) ([]string, error) {
	filename := filepath.Join(p.dir, containerName, date+".txt")
	return scanTextFile(filename, opts)
}

// scanTextFile reads a plain-text log file and returns filtered lines.
// Shared between Persister.Search and the storage viewer.
func scanTextFile(filename string, opts SearchOpts) ([]string, error) {
	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()
	return scanLines(f, opts), nil
}

func scanLines(r io.Reader, opts SearchOpts) []string {
	lowerQuery := strings.ToLower(opts.Query)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	if opts.Tail && opts.Limit > 0 {
		// Ring buffer of the last N matches.
		ring := make([]string, opts.Limit)
		idx, count := 0, 0
		for scanner.Scan() {
			line := scanner.Text()
			if lowerQuery != "" && !strings.Contains(strings.ToLower(line), lowerQuery) {
				continue
			}
			ring[idx] = line
			idx = (idx + 1) % opts.Limit
			count++
		}
		out := make([]string, 0, min(count, opts.Limit))
		if count <= opts.Limit {
			out = append(out, ring[:count]...)
		} else {
			out = append(out, ring[idx:]...)
			out = append(out, ring[:idx]...)
		}
		return out
	}

	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if lowerQuery != "" && !strings.Contains(strings.ToLower(line), lowerQuery) {
			continue
		}
		lines = append(lines, line)
		if opts.Limit > 0 && !opts.Tail && len(lines) >= opts.Limit {
			break
		}
	}
	if lines == nil {
		return []string{}
	}
	return lines
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
