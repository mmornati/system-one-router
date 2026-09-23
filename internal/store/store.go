// Package store appends routing events to a JSONL file: the raw material for the dashboard
// and for re-fitting model skills.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Log struct {
	mu sync.Mutex
	f  *os.File
}

func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f}, nil
}

func (l *Log) Write(kind string, v any) {
	if l == nil {
		return
	}
	b, err := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "kind": kind, "data": v})
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Write(append(b, '\n'))
}

func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}
