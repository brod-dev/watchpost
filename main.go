// watchpost — a tiny self-hosted uptime monitor.
//
// One binary, no database. Point it at your endpoints, open the
// dashboard, and know at a glance what's up.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

const historySize = 60

// Target is one endpoint to watch.
type Target struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Result is the outcome of a single check.
type Result struct {
	OK        bool      `json:"ok"`
	Status    int       `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	At        time.Time `json:"at"`
	Err       string    `json:"err,omitempty"`
}

// Monitor owns the targets and their check history.
type Monitor struct {
	mu       sync.RWMutex
	targets  []Target
	history  map[string][]Result
	interval time.Duration
	client   *http.Client
}

// NewMonitor builds a Monitor for the given targets.
func NewMonitor(targets []Target, interval time.Duration) *Monitor {
	return &Monitor{
		targets:  targets,
		history:  make(map[string][]Result, len(targets)),
		interval: interval,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// check performs one HTTP GET against a target.
func (m *Monitor) check(t Target) Result {
	start := time.Now()
	resp, err := m.client.Get(t.URL)
	r := Result{At: start, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		r.Err = err.Error()
		return r
	}
	defer resp.Body.Close()
	r.Status = resp.StatusCode
	r.OK = resp.StatusCode >= 200 && resp.StatusCode < 400
	r.LatencyMS = time.Since(start).Milliseconds()
	return r
}

// record appends a result, trimming history to historySize.
func (m *Monitor) record(name string, r Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := append(m.history[name], r)
	if len(h) > historySize {
		h = h[len(h)-historySize:]
	}
	m.history[name] = h
}

// checkAll runs every target check concurrently and waits.
func (m *Monitor) checkAll() {
	var wg sync.WaitGroup
	for _, t := range m.targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			m.record(t.Name, m.check(t))
		}(t)
	}
	wg.Wait()
}

// Run checks on a ticker until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	m.checkAll()
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.checkAll()
		}
	}
}

// TargetStatus is the API view of one target.
type TargetStatus struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Up        bool      `json:"up"`
	Status    int       `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	UptimePct float64   `json:"uptime_pct"`
	LastCheck time.Time `json:"last_check"`
	Err       string    `json:"err,omitempty"`
	History   []Result  `json:"history"`
}

// Snapshot produces the current status of every target.
func (m *Monitor) Snapshot() []TargetStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]TargetStatus, 0, len(m.targets))
	for _, t := range m.targets {
		h := m.history[t.Name]
		ts := TargetStatus{Name: t.Name, URL: t.URL, History: h}
		if len(h) > 0 {
			last := h[len(h)-1]
			ts.Up = last.OK
			ts.Status = last.Status
			ts.LatencyMS = last.LatencyMS
			ts.LastCheck = last.At
			ts.Err = last.Err
			ok := 0
			for _, r := range h {
				if r.OK {
					ok++
				}
			}
			ts.UptimePct = 100 * float64(ok) / float64(len(h))
		}
		out = append(out, ts)
	}
	return out
}

func loadConfig(path string) ([]Target, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Targets []Target `json:"targets"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(cfg.Targets) == 0 {
		return nil, fmt.Errorf("%s has no targets", path)
	}
	for i, t := range cfg.Targets {
		if t.URL == "" {
			return nil, fmt.Errorf("target %d has no url", i)
		}
		if t.Name == "" {
			cfg.Targets[i].Name = t.URL
		}
	}
	return cfg.Targets, nil
}

func main() {
	addr := flag.String("addr", ":8080", "address for the dashboard")
	config := flag.String("config", "watchpost.json", "path to config file")
	interval := flag.Duration("interval", 30*time.Second, "time between checks")
	flag.Parse()

	targets, err := loadConfig(*config)
	if err != nil {
		log.Fatalf("watchpost: %v\n\nCreate %s like:\n%s", err, *config,
			`{"targets":[{"name":"My site","url":"https://example.com"}]}`)
	}

	mon := NewMonitor(targets, *interval)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go mon.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboardHTML)
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"interval_sec": int(mon.interval.Seconds()),
			"targets":      mon.Snapshot(),
		})
	})

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Printf("watchpost: watching %d target(s) every %s — dashboard at http://localhost%s", len(targets), *interval, *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
