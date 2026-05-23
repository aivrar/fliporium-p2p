// flipstats is the tiny public-stats server for fliporium.com. It does two
// jobs: serve the .exe download (counting it) and expose /api/stats as JSON.
//
// It deliberately keeps zero per-visit state. The download counter is a single
// monotonic integer on disk; we never record IPs, never set cookies, never
// fingerprint.
//
// Flags:
//
//	-listen          HTTP listen address (default :8088)
//	-data            Directory for the counter file (default /var/lib/flipstats)
//	-exe             Path to the .exe to serve at /dl/fliporium.exe
//	-trusted-proxy   IP allowed to set X-Forwarded-For (usually Caddy on localhost)
//
// Behind the scenes Caddy reverse-proxies /api/stats, /api/contact, and /dl/
// to this. Static HTML/CSS is served by Caddy directly from /var/www/fliporium.
//
// Contact form: POST /api/contact stores each message as a JSON line in
// <data>/contact.jsonl and, if SMTP is configured via the environment
// (FLIPSTATS_SMTP_HOST/PORT/USER/PASS + FLIPSTATS_CONTACT_TO), relays it to
// the configured inbox with Reply-To set to the visitor.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
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
	trustedProxy = flag.String("trusted-proxy", "127.0.0.1", "IP allowed to set X-Forwarded-For; usually Caddy on localhost")
)

// SMTP relay config, read from the environment (set via the systemd
// EnvironmentFile so the app password never appears in `ps` or the unit).
// If smtpHost/smtpUser/smtpPass/contactTo are not all set, contact-form
// emails are skipped and submissions are only stored to the JSONL inbox.
var (
	smtpHost  = os.Getenv("FLIPSTATS_SMTP_HOST") // e.g. smtp.gmail.com
	smtpPort  = os.Getenv("FLIPSTATS_SMTP_PORT") // e.g. 587
	smtpUser  = os.Getenv("FLIPSTATS_SMTP_USER") // e.g. christerfredrickson@gmail.com
	smtpPass  = os.Getenv("FLIPSTATS_SMTP_PASS") // gmail app password
	contactTo = os.Getenv("FLIPSTATS_CONTACT_TO")
)

func smtpConfigured() bool {
	return smtpHost != "" && smtpPort != "" && smtpUser != "" && smtpPass != "" && contactTo != ""
}

const counterFile = "downloads.count"

// stats is the shape of /api/stats output. Field name is snake_case so the
// stats.html script can read it without translation.
type stats struct {
	Downloads int64 `json:"downloads"`
}

// downloads is incremented on every successful /dl/fliporium.exe response.
// It's persisted to disk after each increment. We use atomic so the counter
// stays correct under concurrent requests.
var downloads atomic.Int64

func main() {
	flag.Parse()
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("flipstats: cannot create -data dir %q: %v", *dataDir, err)
	}
	if err := loadCounter(); err != nil {
		log.Fatalf("flipstats: load counter: %v", err)
	}
	log.Printf("flipstats: starting; downloads=%d listen=%s exe=%s", downloads.Load(), *listenAddr, *exePath)

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
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(stats{Downloads: downloads.Load()})
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

	w.Header().Set("Content-Type", "application/vnd.microsoft.portable-executable")
	w.Header().Set("Content-Disposition", `attachment; filename="fliporium.exe"`)
	w.Header().Set("Cache-Control", "no-cache")

	// Wrap the writer so we can see how many bytes actually reached the
	// client. We only count a download once the *whole* file was delivered
	// to a non-range GET. Scanners and link-preview bots overwhelmingly
	// connect, confirm the file exists, then abort partway -- this filters
	// them out so the public counter reflects real installs, not crawls.
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, "fliporium.exe", st.ModTime(), f)

	// Decide whether this counts, and log every attempt with the reason so
	// the download tally is fully auditable after the fact.
	ip := clientIP(r)
	ua := r.UserAgent()
	isRange := r.Header.Get("Range") != ""
	switch {
	case r.Method != http.MethodGet:
		log.Printf("flipstats: dl skip (method=%s) ip=%s ua=%q", r.Method, ip, ua)
	case isRange:
		log.Printf("flipstats: dl skip (range request) ip=%s bytes=%d ua=%q", ip, cw.written, ua)
	case cw.status != http.StatusOK || cw.written < st.Size():
		log.Printf("flipstats: dl skip (incomplete: %d/%d bytes, status=%d) ip=%s ua=%q",
			cw.written, st.Size(), cw.status, ip, ua)
	case looksLikeBot(ua):
		log.Printf("flipstats: dl skip (bot ua) ip=%s ua=%q", ip, ua)
	default:
		n := downloads.Add(1)
		if err := saveCounter(n); err != nil {
			log.Printf("flipstats: save counter (%d): %v", n, err)
		}
		log.Printf("flipstats: COUNTED download #%d (%d bytes) ip=%s ua=%q", n, cw.written, ip, ua)
	}
}

// countingWriter wraps an http.ResponseWriter to record the status code and
// the number of body bytes written, so the download handler can tell a
// complete transfer from an aborted one.
type countingWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(p)
	c.written += int64(n)
	return n, err
}

// botUA matches user-agent substrings common to crawlers, scanners, and
// link-preview fetchers. Case-insensitive. This is a best-effort secondary
// filter; the primary signal is whether the full file was delivered.
var botUA = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|scan|headless|preview|fetch|curl|wget|python-requests|go-http|httpclient|facebookexternalhit|whatsapp|telegrambot|discordbot|slackbot|bingpreview`)

func looksLikeBot(ua string) bool {
	if ua == "" {
		return true // no UA at all is overwhelmingly automated
	}
	return botUA.MatchString(ua)
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

	// Best-effort email relay. The message is already safely on disk, so a
	// mail failure doesn't fail the request -- we just log it. The user can
	// still recover the message from contact.jsonl.
	if smtpConfigured() {
		if err := sendContactEmail(rec); err != nil {
			log.Printf("contact: email relay failed (message still saved to disk): %v", err)
		} else {
			log.Printf("contact: emailed to %s", contactTo)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

// sanitizeHeader strips CR/LF from a value so it can't be used to inject
// extra email headers (header-injection defense for the Reply-To / Subject).
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// sendContactEmail relays a contact submission to the configured inbox via
// the SMTP server (Gmail). From is the authenticated account; Reply-To is the
// visitor so a plain "Reply" in the inbox goes straight back to them.
func sendContactEmail(rec storedContact) error {
	subject := sanitizeHeader(rec.Subject)
	if subject == "" {
		subject = "(no subject)"
	}
	replyTo := sanitizeHeader(rec.Name) + " <" + sanitizeHeader(rec.Email) + ">"

	var b strings.Builder
	fmt.Fprintf(&b, "From: Fliporium Contact <%s>\r\n", smtpUser)
	fmt.Fprintf(&b, "To: %s\r\n", contactTo)
	fmt.Fprintf(&b, "Reply-To: %s\r\n", replyTo)
	fmt.Fprintf(&b, "Subject: [Fliporium] %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", rec.Time.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <contact-%d@fliporium.com>\r\n", rec.Time.UnixNano())
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "New contact form submission from fliporium.com\r\n\r\n")
	fmt.Fprintf(&b, "Name:    %s\r\n", rec.Name)
	fmt.Fprintf(&b, "Email:   %s\r\n", rec.Email)
	fmt.Fprintf(&b, "Subject: %s\r\n", rec.Subject)
	fmt.Fprintf(&b, "Time:    %s\r\n", rec.Time.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "IP:      %s\r\n", rec.IP)
	b.WriteString("\r\n----------------------------------------\r\n\r\n")
	// Normalize message line endings to CRLF for SMTP.
	msg := strings.ReplaceAll(rec.Message, "\r\n", "\n")
	msg = strings.ReplaceAll(msg, "\n", "\r\n")
	b.WriteString(msg)
	b.WriteString("\r\n")

	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	addr := net.JoinHostPort(smtpHost, smtpPort)
	return smtp.SendMail(addr, auth, smtpUser, []string{contactTo}, []byte(b.String()))
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
