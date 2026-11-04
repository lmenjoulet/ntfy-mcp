//go:build ignore

// Fake ntfy server used by scripts/run-tests.sh for hermetic end-to-end
// tests: no real ntfy server, token or network access needed.
//
// It stores POSTed messages in memory and serves them back as
// newline-delimited JSON on GET /<topic>, mimicking the ntfy API
// (poll/since/cached query parameters). If the EXPECT_TOKEN environment
// variable is set, requests must carry "Authorization: Bearer <EXPECT_TOKEN>".
//
// Run it with: go run scripts/fake-ntfy.go -addr 127.0.0.1:18099
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type message struct {
	ID       string   `json:"id"`
	Time     int64    `json:"time"`
	Event    string   `json:"event"`
	Topic    string   `json:"topic"`
	Message  string   `json:"message"`
	Title    string   `json:"title,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

var (
	mu          sync.Mutex
	messages    []message
	seq         int
	expectToken = os.Getenv("EXPECT_TOKEN")
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18099", "listen address")
	flag.Parse()
	http.HandleFunc("/", handler)
	log.Printf("fake ntfy listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func handler(w http.ResponseWriter, r *http.Request) {
	if expectToken != "" && r.Header.Get("Authorization") != "Bearer "+expectToken {
		log.Printf("rejected %s %s: HTTP 401", r.Method, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
		return
	}

	topic := strings.Trim(r.URL.Path, "/")
	switch r.Method {
	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seq++
		msg := message{
			ID:       fmt.Sprintf("id-%d", seq),
			Time:     time.Now().Unix(),
			Event:    "message",
			Topic:    topic,
			Message:  string(body),
			Title:    r.Header.Get("Title"),
			Priority: atoi(r.Header.Get("Priority")),
		}
		if tags := r.Header.Get("Tags"); tags != "" {
			msg.Tags = strings.Split(tags, ",")
		}
		messages = append(messages, msg)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": msg.ID, "topic": topic}) //nolint:errcheck

	case http.MethodGet:
		since := r.URL.Query().Get("since")
		mu.Lock()
		var out []message
		for _, m := range messages {
			if m.Topic != topic {
				continue
			}
			if since != "" && since != "all" {
				if ts, err := strconv.ParseInt(since, 10, 64); err == nil {
					if m.Time <= ts {
						continue
					}
				} else if m.ID == since {
					continue
				}
			}
			out = append(out, m)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		for _, m := range out {
			json.NewEncoder(w).Encode(m) //nolint:errcheck
		}

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
