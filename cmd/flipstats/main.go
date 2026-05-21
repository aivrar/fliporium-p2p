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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	trustedProxy = flag.String("trusted-proxy", "127.0.0.1", "IP allowed to set X-Forwarded-For; usually Caddy on localhost")
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
	mux.HandleFunc("/api/contact", contactHandler)
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

// ---------- /api/contact ----------

// contactSubmission is what the form POSTs as JSON.
type contactSubmission struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Subject string `json:"subject"`
	Message string `json:"message"`
	Phone   string `json:"phone"` // honeypot -- must be empty
}

// storedContact is what we persist on disk. One JSON object per line in
// contact.jsonl so the user can `tail` or `jq` the file directly.
type storedContact struct {
	Time    time.Time `json:"time"`
	IP      string    `json:"ip"`
	Name    string    `json:"name"`
	Email   string    `json:"email"`
	Subject string    `json:"subject"`
	Message string    `json:"message"`
}

var emailRE = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// contactRate tracks per-IP submissions for rate limiting.
var (
	contactRateMu sync.Mutex
	contactRate   = map[string][]time.Time{}
)

const (
	contactRateWindow = 1 * time.Hour
	contactRateLimit  = 3
)

// allowContact returns true if the given IP hasn't exceeded the contact form
// rate limit in the trailing window. It also prunes old entries.
func allowContact(ip string) bool {
	contactRateMu.Lock()
	defer contactRateMu.Unlock()
	now := time.Now()
	cutoff := now.Add(-contactRateWindow)
	hits := contactRate[ip]
	pruned := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	if len(pruned) >= contactRateLimit {
		contactRate[ip] = pruned
		return false
	}
	contactRate[ip] = append(pruned, now)
	return true
}

// clientIP returns the requester's IP. Behind Caddy we get the original IP
// via X-Forwarded-For (Caddy sets this), but we only trust that header when
// the immediate peer is the configured trusted proxy.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == *trustedProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// XFF can be a comma-separated chain. The leftmost is the original.
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	return host
}

func contactHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Hard cap on body size: 16 KB is plenty for name + email + subject + 5KB message.
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var sub contactSubmission
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		http.Error(w, "could not parse JSON body", http.StatusBadRequest)
		return
	}

	// Honeypot: dumb bots fill every field. Silently 200 so they don't retry.
	if strings.TrimSpace(sub.Phone) != "" {
		log.Printf("contact: honeypot tripped from %s (silently accepted)", clientIP(r))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}

	sub.Name = strings.TrimSpace(sub.Name)
	sub.Email = strings.TrimSpace(sub.Email)
	sub.Subject = strings.TrimSpace(sub.Subject)
	sub.Message = strings.TrimSpace(sub.Message)

	if l := len(sub.Name); l == 0 || l > 100 {
		http.Error(w, "name must be 1-100 characters", http.StatusBadRequest)
		return
	}
	if l := len(sub.Email); l == 0 || l > 120 || !emailRE.MatchString(sub.Email) {
		http.Error(w, "valid email is required", http.StatusBadRequest)
		return
	}
	if l := len(sub.Subject); l > 200 {
		http.Error(w, "subject must be at most 200 characters", http.StatusBadRequest)
		return
	}
	if l := len(sub.Message); l < 10 || l > 5000 {
		http.Error(w, "message must be 10-5000 characters", http.StatusBadRequest)
		return
	}

	ip := clientIP(r)
	if !allowContact(ip) {
		http.Error(w, "rate limit exceeded -- try again later", http.StatusTooManyRequests)
		return
	}

	rec := storedContact{
		Time:    time.Now().UTC(),
		IP:      ip,
		Name:    sub.Name,
		Email:   sub.Email,
		Subject: sub.Subject,
		Message: sub.Message,
	}
	if err := appendContact(rec); err != nil {
		log.Printf("contact: persist failed: %v", err)
		http.Error(w, "could not save message", http.StatusInternalServerError)
		return
	}
	log.Printf("contact: from %s <%s> subject=%q ip=%s len=%d", sub.Name, sub.Email, sub.Subject, ip, len(sub.Message))

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

// appendContact writes one JSON line to <data>/contact.jsonl. Atomic per
// line because we open with O_APPEND and a single Write of the encoded bytes.
func appendContact(rec storedContact) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := filepath.Join(*dataDir, "contact.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
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
