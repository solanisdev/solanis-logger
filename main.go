package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func main() {
	logsDir := os.Getenv("LOGS_DIR")
	if logsDir == "" {
		logsDir = "./logs"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	persister := NewPersister(logsDir)
	collector := NewCollector(persister)

	ctx := context.Background()
	collector.Start(ctx)

	if pm2Dir := os.Getenv("PM2_LOGS_DIR"); pm2Dir != "" {
		NewPM2Collector(collector, pm2Dir).Start(ctx)
	}

	auth := NewAuthManager()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /login", auth.HandleLoginPage)
	mux.HandleFunc("POST /login", auth.HandleLoginSubmit)
	mux.HandleFunc("POST /logout", auth.HandleLogout)

	staticHandler := http.FileServer(http.Dir("static"))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" || r.URL.Path == "/storage.html" || r.URL.Path == "/view.html" {
			if !auth.IsAuthenticated(r) {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
		}
		staticHandler.ServeHTTP(w, r)
	}))

	mux.Handle("/api/containers", auth.RequireAuth(gzipMiddleware(handleContainers(collector, persister))))
	mux.Handle("/api/logs/stream", auth.RequireAuth(handleStream(collector)))
	mux.Handle("/api/logs/history", auth.RequireAuth(gzipMiddleware(handleHistory(persister))))
	mux.Handle("/api/logs/dates", auth.RequireAuth(gzipMiddleware(handleDates(persister))))
	mux.Handle("/api/storage/files", auth.RequireAuth(gzipMiddleware(handleStorageFiles(logsDir))))
	mux.Handle("/api/storage/zip", auth.RequireAuth(handleStorageZip(logsDir, persister)))
	mux.Handle("/api/storage/download", auth.RequireAuth(handleStorageDownload(logsDir)))
	mux.Handle("/api/storage/view", auth.RequireAuth(gzipMiddleware(handleStorageView(logsDir))))
	mux.Handle("DELETE /api/storage/file", auth.RequireAuth(handleStorageDelete(logsDir, persister)))
	mux.Handle("/healthz", handleHealthz(collector, persister))

	log.Printf("logger listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func handleContainers(collector *Collector, persister *Persister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type resp struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Source string `json:"source"`
		}

		live := collector.GetContainers()
		liveSet := make(map[string]bool, len(live))
		result := make([]resp, 0, len(live))
		for _, c := range live {
			result = append(result, resp{Name: c.Name, Status: c.Status, Source: c.Source})
			liveSet[c.Name] = true
		}

		known, _ := persister.ListKnownContainers()
		for _, name := range known {
			if !liveSet[name] {
				src := ""
				if strings.HasPrefix(name, "pm2:") {
					src = "pm2"
				}
				result = append(result, resp{Name: name, Status: "stopped", Source: src})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func handleStream(collector *Collector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		subID, ch := collector.Subscribe(name)
		defer collector.Unsubscribe(name, subID)

		ctx := r.Context()
		for {
			select {
			case line := <-ch:
				data, _ := json.Marshal(line)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-ctx.Done():
				return
			}
		}
	}
}

func handleHistory(persister *Persister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		date := r.URL.Query().Get("date")

		if name == "" || date == "" {
			http.Error(w, "name and date required", http.StatusBadRequest)
			return
		}

		opts := SearchOpts{Query: r.URL.Query().Get("q")}
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				opts.Limit = n
			}
		}
		opts.Tail = r.URL.Query().Get("tail") == "1" || r.URL.Query().Get("tail") == "true"

		lines, err := persister.Search(name, date, opts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(lines)
	}
}

func handleDates(persister *Persister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}

		dates, err := persister.ListDates(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dates)
	}
}

func handleHealthz(collector *Collector, persister *Persister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type resp struct {
			Status        string  `json:"status"`
			UptimeSeconds float64 `json:"uptime_seconds"`
			Dropped       uint64  `json:"dropped_lines"`
			Containers    int     `json:"live_containers"`
			RetentionDays int     `json:"retention_days"`
			MaxLogLines   int     `json:"max_log_lines"`
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp{
			Status:        "ok",
			UptimeSeconds: persister.Uptime().Seconds(),
			Dropped:       persister.DroppedCount(),
			Containers:    len(collector.GetContainers()),
			RetentionDays: persister.RetentionDays(),
			MaxLogLines:   persister.maxLines,
		})
	}
}

// gzipMiddleware compresses responses when the client accepts gzip.
// Not safe to use on streaming (SSE) or binary-download endpoints.
func gzipMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		h.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, w: gz}, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	w io.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.w.Write(b)
}
