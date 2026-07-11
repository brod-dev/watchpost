package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckUpAndDown(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer down.Close()

	m := NewMonitor([]Target{{Name: "up", URL: up.URL}, {Name: "down", URL: down.URL}}, time.Minute)
	m.checkAll()

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("want 2 targets, got %d", len(snap))
	}
	for _, s := range snap {
		switch s.Name {
		case "up":
			if !s.Up || s.Status != 200 || s.UptimePct != 100 {
				t.Errorf("up target wrong: %+v", s)
			}
		case "down":
			if s.Up || s.Status != 503 || s.UptimePct != 0 {
				t.Errorf("down target wrong: %+v", s)
			}
		}
	}
}

func TestHistoryTrim(t *testing.T) {
	m := NewMonitor([]Target{{Name: "x", URL: "http://x"}}, time.Minute)
	for i := 0; i < historySize+25; i++ {
		m.record("x", Result{OK: true, At: time.Now()})
	}
	m.mu.RLock()
	n := len(m.history["x"])
	m.mu.RUnlock()
	if n != historySize {
		t.Errorf("history not trimmed: %d", n)
	}
}

func TestUnreachable(t *testing.T) {
	m := NewMonitor([]Target{{Name: "gone", URL: "http://127.0.0.1:1"}}, time.Minute)
	m.checkAll()
	s := m.Snapshot()[0]
	if s.Up || s.Err == "" {
		t.Errorf("unreachable target should be down with an error, got %+v", s)
	}
}
