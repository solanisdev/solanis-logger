package main

import (
	"archive/zip"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".txt") {
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

func handleStorageZip(logsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Paths) == 0 {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		absLogsDir := filepath.Clean(logsDir)
		var absFiles []string
		for _, p := range req.Paths {
			p = filepath.FromSlash(p)
			if strings.Contains(p, "..") {
				http.Error(w, "invalid path: "+p, http.StatusBadRequest)
				return
			}
			if !strings.HasSuffix(p, ".txt") {
				http.Error(w, "only .txt files allowed: "+p, http.StatusBadRequest)
				return
			}
			abs := filepath.Clean(filepath.Join(absLogsDir, p))
			if !strings.HasPrefix(abs, absLogsDir+string(os.PathSeparator)) {
				http.Error(w, "path escapes log directory: "+p, http.StatusBadRequest)
				return
			}
			absFiles = append(absFiles, abs)
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="logs-archive.zip"`)

		zw := zip.NewWriter(w)
		var zipped []string
		for _, abs := range absFiles {
			rel, _ := filepath.Rel(absLogsDir, abs)
			rel = filepath.ToSlash(rel)
			f, err := os.Open(abs)
			if err != nil {
				log.Printf("storage zip: open %s: %v", abs, err)
				continue
			}
			ew, err := zw.CreateHeader(&zip.FileHeader{
				Name:   rel,
				Method: zip.Deflate,
			})
			if err != nil {
				f.Close()
				log.Printf("storage zip: create header %s: %v", rel, err)
				continue
			}
			if _, err := io.Copy(ew, f); err != nil {
				f.Close()
				log.Printf("storage zip: copy %s: %v", rel, err)
				continue
			}
			f.Close()
			zipped = append(zipped, abs)
		}

		if err := zw.Close(); err != nil {
			log.Printf("storage zip: close writer: %v", err)
		}

		dirs := make(map[string]struct{})
		for _, abs := range zipped {
			if err := os.Remove(abs); err != nil {
				log.Printf("storage zip: remove %s: %v", abs, err)
			} else {
				dirs[filepath.Dir(abs)] = struct{}{}
			}
		}
		for dir := range dirs {
			if err := os.Remove(dir); err == nil {
				log.Printf("storage zip: removed empty dir %s", dir)
			}
		}
	}
}
