package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// events.go streams the run as JSON Lines while it happens.
//
// Written as it runs rather than summarised at the end for two reasons.
// A full-corpus ingest takes minutes, and a progress bar that only moves
// when a document finishes tells you nothing about where the time went
// inside it. And a run that dies halfway still leaves everything it
// learned on disk, which is the difference between a wasted twenty
// minutes and a partial result.
//
// One event per line, flushed immediately: a dashboard can tail the file
// and render as it arrives, with no coordination beyond the filesystem.

type event struct {
	T     string  `json:"t"`               // event type
	Time  float64 `json:"time"`            // seconds since run start
	Doc   string  `json:"doc,omitempty"`   // document
	Arm   string  `json:"arm,omitempty"`   // "jev" | "generative"
	Phase string  `json:"phase,omitempty"` // parse | detect | verify

	Pages    int     `json:"pages,omitempty"`
	Requests int     `json:"requests,omitempty"`
	Seconds  float64 `json:"seconds,omitempty"`
	InTok    int     `json:"in_tokens,omitempty"`
	OutTok   int     `json:"out_tokens,omitempty"`
	CostUSD  float64 `json:"cost_usd,omitempty"`
	Found    []int   `json:"found,omitempty"`
	Err      string  `json:"err,omitempty"`
	Note     string  `json:"note,omitempty"`
}

type emitter struct {
	mu    sync.Mutex
	f     *os.File
	enc   *json.Encoder
	start time.Time
}

func newEmitter(path string) (*emitter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &emitter{f: f, enc: json.NewEncoder(f), start: time.Now()}, nil
}

// emit writes one event. Safe for concurrent use — documents are
// processed in parallel, which is the point of the measurement.
func (e *emitter) emit(ev event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ev.Time = time.Since(e.start).Seconds()
	_ = e.enc.Encode(ev)
	_ = e.f.Sync() // a tailing dashboard should see it now, not at close
}

func (e *emitter) close() { _ = e.f.Close() }

func (e *emitter) elapsed() time.Duration { return time.Since(e.start) }

// dotEnv reads one key from a gitignored .env up-tree, so credentials
// stay out of command lines and shell history.
func dotEnv(key string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 6; i++ {
		if v := readEnvFile(filepath.Join(dir, ".env"), key); v != "" {
			return v
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func readEnvFile(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(name) == key {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}
