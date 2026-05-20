// flipstats is the tiny public-stats server for fliporium.com. It does two
// jobs: serve the .exe download (counting it) and expose /api/stats as JSON.
//
// It deliberately keeps zero per-visit state. The download counter is a single
// monotonic integer on disk; we never record IPs, never set cookies, never
// fingerprint. The node counts come from `headscale nodes list -o json` run
// every 30 seconds.
//
// Flags:
//
//	-listen          HTTP listen address (default :8088)
//	-data            Directory for the counter file (default /var/lib/flipstats)
//	-exe             Path to the .exe to serve at /dl/fliporium.exe
//	-headscale       Path to the headscale binary (default /usr/local/bin/headscale)
//	-headscale-cfg   Path to the headscale config (default /etc/headscale/config.yaml)
//
// Behind the scenes Caddy reverse-proxies /api/stats and /dl/ to this. Static
// HTML/CSS is served by Caddy directly from /var/www/fliporium.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	listenAddr   = flag.String("listen", ":8088", "HTTP listen address")
	dataDir      = flag.String("data", "/var/lib/flipstats", "Directory for the persistent counter")
	exePath      = flag.String("exe", "/var/www/fliporium/dl/fliporium.exe", "Path to the .exe to serve")
	headscaleBin = flag.String("headscale", "/usr/local/bin/headscale", "Path to headscale binary")
	headscaleCfg = flag.String("headscale-cfg", "/etc/headscale/config.yaml", "Path to headscale config")
)

const (
	counterFile  = "downloads.count"
	pollInterval = 30 * time.Second
	// A node is "online_now" if Headscale has heard from it inside this window.
	onlineWindow = 5 * time.Minute
)

// stats is the shape of /api/stats output. Field names are snake_case so the
// stats.html script can read them without translation.
type stats struct {
	Downloads        int64 `json:"downloads"`
	OnlineNow        int   `json:"online_now"`
	RegisteredTotal  int   `json:"registered_total"`
	RefreshedAt      int64 `json:"refreshed_at"`
}

// downloads is incremented on every successful /dl/fliporium.exe response.
// It's persisted to disk after each increment. We use atomic so the counter
// stays correct under concurrent requests.
var downloads atomic.Int64

// nodeCounts is the cached output of the most recent headscale poll. Updated
// in a background goroutine so /api/stats is always cheap (no fork+exec
// per request).
var (
	nodeMu          sync.RWMutex
	cachedOnline    int
	cachedTotal     int
	cachedAt        time.Time
)

func main() {
	flag.Parse()
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("flipstats: cannot create -data dir %q: %v", *dataDir, err)
	}
	if err := loadCounter(); err != nil {
		log.Fatalf("flipstats: load counter: %v", err)
	}
	log.Printf("flipstats: starting; downloads=%d listen=%s exe=%s", downloads.Load(), *listenAddr, *exePath)

	// Prime the headscale cache once before serving, so the first /api/stats
	// response after startup isn't blank.
	if err := refreshNodes(); err != nil {
		log.Printf("flipstats: initial headscale poll failed (%v); /api/stats will report 0s until next tick", err)
	}
	go pollLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", statsHandler)
	mux.HandleFunc("/dl/fliporium.exe", downloadHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("flipstats: ready on %s", *listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("flipstats: serve: %v", err)
	}
}

// ---------- handlers ----------

func statsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	nodeMu.RLock()
	online := cachedOnline
	total := cachedTotal
	at := cachedAt
	nodeMu.RUnlock()

	s := stats{
		Downloads:       downloads.Load(),
		OnlineNow:       online,
		RegisteredTotal: total,
		RefreshedAt:     at.Unix(),
	}
	// Cache for 30s on the edge -- the underlying numbers change slowly and we
	// don't want anyone hammering Headscale via this endpoint.
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(s)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f, err := os.Open(*exePath)
	if err != nil {
		log.Printf("flipstats: cannot open exe %q: %v", *exePath, err)
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}

	// Count HEAD requests separately from GETs: HEAD is what redirect-checkers
	// and link-preview bots do, and we only want to count actual file pulls.
	if r.Method == http.MethodGet {
		n := downloads.Add(1)
		if err := saveCounter(n); err != nil {
			log.Printf("flipstats: save counter (%d): %v", n, err)
		}
	}

	w.Header().Set("Content-Type", "application/vnd.microsoft.portable-executable")
	w.Header().Set("Content-Disposition", `attachment; filename="fliporium.exe"`)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "fliporium.exe", st.ModTime(), f)
}

// ---------- counter persistence ----------

func loadCounter() error {
	path := filepath.Join(*dataDir, counterFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			downloads.Store(0)
			return nil
		}
		return err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return fmt.Errorf("counter file %q corrupt: %w", path, err)
	}
	downloads.Store(n)
	return nil
}

func saveCounter(n int64) error {
	path := filepath.Join(*dataDir, counterFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n, 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- headscale polling ----------

// protoTimestamp matches Headscale's google.protobuf.Timestamp JSON shape.
// The headscale CLI emits {"seconds": int64, "nanos": int32}, not RFC3339.
type protoTimestamp struct {
	Seconds int64 `json:"seconds"`
	Nanos   int32 `json:"nanos"`
}

func (t protoTimestamp) Time() time.Time {
	if t.Seconds <= 0 {
		// Headscale uses {"seconds": -62135596800} to mean "unset" (Go's zero
		// time). Treat as never-seen, well in the past.
		return time.Time{}
	}
	return time.Unix(t.Seconds, int64(t.Nanos))
}

// headscaleNode is the subset of the headscale JSON output we care about.
type headscaleNode struct {
	ID        int            `json:"id"`
	Name      string         `json:"name"`
	GivenName string         `json:"given_name"`
	LastSeen  protoTimestamp `json:"last_seen"`
}

func pollLoop() {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for range t.C {
		if err := refreshNodes(); err != nil {
			log.Printf("flipstats: headscale poll: %v", err)
		}
	}
}

func refreshNodes() error {
	out, err := exec.Command(*headscaleBin, "-c", *headscaleCfg, "nodes", "list", "-o", "json").Output()
	if err != nil {
		return fmt.Errorf("headscale exec: %w", err)
	}
	var nodes []headscaleNode
	if err := json.Unmarshal(out, &nodes); err != nil {
		return fmt.Errorf("headscale json parse: %w", err)
	}
	now := time.Now()
	online := 0
	for _, n := range nodes {
		last := n.LastSeen.Time()
		if !last.IsZero() && now.Sub(last) <= onlineWindow {
			online++
		}
	}
	nodeMu.Lock()
	cachedOnline = online
	cachedTotal = len(nodes)
	cachedAt = now
	nodeMu.Unlock()
	return nil
}
