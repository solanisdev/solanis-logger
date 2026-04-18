package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
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

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("static")))
	mux.HandleFunc("/api/containers", handleContainers(collector, persister))
	mux.HandleFunc("/api/logs/stream", handleStream(collector))
	mux.HandleFunc("/api/logs/history", handleHistory(persister))
	mux.HandleFunc("/api/logs/dates", handleDates(persister))

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
		}

		live := collector.GetContainers()
		liveSet := make(map[string]bool, len(live))
		result := make([]resp, 0, len(live))
		for _, c := range live {
			result = append(result, resp{Name: c.Name, Status: c.Status})
			liveSet[c.Name] = true
		}

		known, _ := persister.ListKnownContainers()
		for _, name := range known {
			if !liveSet[name] {
				result = append(result, resp{Name: name, Status: "stopped"})
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
		query := r.URL.Query().Get("q")

		if name == "" || date == "" {
			http.Error(w, "name and date required", http.StatusBadRequest)
			return
		}

		lines, err := persister.Search(name, date, query)
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
