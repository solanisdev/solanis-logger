package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type storageFile struct {
	Path   string  `json:"path"`
	SizeMB float64 `json:"size_mb"`
}

func handleStorageFiles(logsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var files []storageFile
		err := filepath.WalkDir(logsDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
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
				return err
			}
			rel, err := filepath.Rel(logsDir, path)
			if err != nil {
				return err
			}
			files = append(files, storageFile{
				Path:   filepath.ToSlash(rel),
				SizeMB: float64(info.Size()) / 1_048_576,
			})
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if files == nil {
			files = []storageFile{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(files)
	}
}

// safePath validates a user-provided relative path and returns its absolute
// form inside logsDir. Rejects traversal attempts and paths that escape the
// logs directory.
func safePath(logsDir, relPath string) (string, error) {
	p := filepath.FromSlash(relPath)
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("invalid path: %s", relPath)
	}
	absLogsDir := filepath.Clean(logsDir)
	abs := filepath.Clean(filepath.Join(absLogsDir, p))
	if !strings.HasPrefix(abs, absLogsDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes log directory: %s", relPath)
	}
	return abs, nil
}

func handleStorageZip(logsDir string, persister *Persister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Paths) == 0 {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		type targetFile struct {
			abs       string
			rel       string
			container string
			date      string
		}
		var targets []targetFile
		for _, p := range req.Paths {
			if !strings.HasSuffix(p, ".txt") {
				http.Error(w, "only .txt files allowed: "+p, http.StatusBadRequest)
				return
			}
			abs, err := safePath(logsDir, p)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			container, date := parseContainerDate(p)
			targets = append(targets, targetFile{
				abs:       abs,
				rel:       filepath.ToSlash(p),
				container: container,
				date:      date,
			})
		}

		// Hand off any active day files from the persister before touching them.
		for _, t := range targets {
			if t.container != "" && t.date != "" {
				persister.RotateActive(t.container, t.date)
			}
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="logs-archive.zip"`)

		zw := zip.NewWriter(w)
		var zipped []targetFile
		for _, t := range targets {
			f, err := os.Open(t.abs)
			if err != nil {
				log.Printf("storage zip: open %s: %v", t.abs, err)
				continue
			}
			ew, err := zw.CreateHeader(&zip.FileHeader{
				Name:   t.rel,
				Method: zip.Deflate,
			})
			if err != nil {
				f.Close()
				log.Printf("storage zip: create header %s: %v", t.rel, err)
				continue
			}
			if _, err := io.Copy(ew, f); err != nil {
				f.Close()
				log.Printf("storage zip: copy %s: %v", t.rel, err)
				continue
			}
			f.Close()
			zipped = append(zipped, t)
		}

		if err := zw.Close(); err != nil {
			log.Printf("storage zip: close writer: %v", err)
		}

		today := time.Now().UTC().Format("2006-01-02")
		dirs := make(map[string]struct{})
		for _, t := range zipped {
			if err := os.Remove(t.abs); err != nil {
				log.Printf("storage zip: remove %s: %v", t.abs, err)
				continue
			}
			if t.date == today {
				if f, err := os.OpenFile(t.abs, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); err != nil {
					log.Printf("storage zip: recreate empty %s: %v", t.abs, err)
				} else {
					f.Close()
				}
			} else {
				dirs[filepath.Dir(t.abs)] = struct{}{}
			}
		}
		for dir := range dirs {
			if err := os.Remove(dir); err == nil {
				log.Printf("storage zip: removed empty dir %s", dir)
			}
		}
	}
}

func handleStorageDownload(logsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if p == "" {
			http.Error(w, "path required", http.StatusBadRequest)
			return
		}
		if !strings.HasSuffix(p, ".zip") {
			http.Error(w, "only .zip downloads allowed", http.StatusBadRequest)
			return
		}
		abs, err := safePath(logsDir, p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		f, err := os.Open(abs)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(abs)+`"`)
		io.Copy(w, f)
	}
}

// handleStorageDelete removes a single .txt or .zip file. For today's active
// .txt it hands the handle off to the persister first, then recreates the file
// empty so logging continues seamlessly. For zips and older .txt files it just
// deletes.
func handleStorageDelete(logsDir string, persister *Persister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if p == "" {
			http.Error(w, "path required", http.StatusBadRequest)
			return
		}
		if !strings.HasSuffix(p, ".txt") && !strings.HasSuffix(p, ".zip") {
			http.Error(w, "only .txt or .zip allowed", http.StatusBadRequest)
			return
		}
		abs, err := safePath(logsDir, p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		container, date := parseContainerDate(p)
		today := time.Now().UTC().Format("2006-01-02")
		if container != "" && date != "" {
			persister.RotateActive(container, date)
		}

		if err := os.Remove(abs); err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Recreate empty .txt only for today's active day so the persister
		// keeps writing without a process restart.
		if strings.HasSuffix(p, ".txt") && date == today {
			if f, err := os.OpenFile(abs, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); err == nil {
				f.Close()
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleStorageView returns filtered lines from a .txt file or from the single
// text entry inside a .zip produced by rotation.
// Query params: path (required), q, limit (int), tail (bool).
func handleStorageView(logsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if p == "" {
			http.Error(w, "path required", http.StatusBadRequest)
			return
		}
		if !strings.HasSuffix(p, ".txt") && !strings.HasSuffix(p, ".zip") {
			http.Error(w, "only .txt or .zip allowed", http.StatusBadRequest)
			return
		}
		abs, err := safePath(logsDir, p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		opts := SearchOpts{Query: r.URL.Query().Get("q")}
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				opts.Limit = n
			}
		}
		opts.Tail = r.URL.Query().Get("tail") == "1" || r.URL.Query().Get("tail") == "true"

		var lines []string
		if strings.HasSuffix(p, ".txt") {
			lines, err = scanTextFile(abs, opts)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			lines, err = readZipEntry(abs, opts)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(lines)
	}
}

// readZipEntry opens a zip and returns filtered lines from its first file
// entry. Rotation-produced zips contain exactly one .txt entry.
func readZipEntry(zipPath string, opts SearchOpts) ([]string, error) {
	rc, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	for _, zf := range rc.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		f, err := zf.Open()
		if err != nil {
			return nil, err
		}
		lines := scanLines(f, opts)
		f.Close()
		return lines, nil
	}
	return []string{}, nil
}

// parseContainerDate extracts the container name and YYYY-MM-DD date from a
// relative path of the form "<container>/<YYYY-MM-DD>.txt". Returns empty
// strings if the path does not match that shape.
func parseContainerDate(relPath string) (string, string) {
	rel := filepath.ToSlash(relPath)
	parts := strings.Split(rel, "/")
	if len(parts) != 2 {
		return "", ""
	}
	name := strings.TrimSuffix(parts[1], ".txt")
	if len(name) != 10 || name[4] != '-' || name[7] != '-' {
		return "", ""
	}
	if _, err := time.Parse("2006-01-02", name); err != nil {
		return "", ""
	}
	return parts[0], name
}
