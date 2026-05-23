package main

// Link unfurling. Only the SENDER of a message runs this — it fetches the
// linked page's Open Graph metadata and bakes a small self-contained card
// (title, description, and a downscaled JPEG thumbnail as a data: URI) into a
// MESSAGE_CARD that rides peer-to-peer to recipients. Recipients render the
// card from the baked-in bytes and never contact the third-party site, so a
// link can't be used to silently harvest a room's IP addresses, and nothing
// touches Fliporium's server. YouTube is handled specially: we keep only the
// video id + a thumbnail, and the frontend loads the actual player (from
// youtube-nocookie.com) only when someone clicks play.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	xhtml "golang.org/x/net/html"

	"fliporium/internal/peer"
)

const unfurlUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

var urlRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// firstURL returns the first http(s) URL in text (trailing punctuation
// trimmed), or "" if there is none.
func firstURL(text string) string {
	m := urlRe.FindString(text)
	if m == "" {
		return ""
	}
	return strings.TrimRight(m, ".,!?:;)]}'\"")
}

func unfurlClient() *http.Client {
	// Reject connections to non-public addresses at dial time. The dialer's
	// Control hook runs AFTER DNS resolution, on the concrete IP it's about to
	// connect to — so this covers redirects and DNS-rebinding tricks, not just
	// the literal host the user typed.
	dialer := &net.Dialer{
		Timeout: 8 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return guardDialAddr(address)
		},
	}
	return &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			MaxIdleConns:          4,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 6 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}

// guardDialAddr blocks outbound unfurl connections to addresses that aren't
// publicly routable. Without it, a link a user is tricked into posting could
// make their machine probe localhost, cloud metadata (169.254.169.254), or the
// LAN and leak the result back into chat as a preview card (SSRF).
func guardDialAddr(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("bad dial address %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("refusing to dial unresolved host %q", host)
	}
	if isNonPublicIP(ip) {
		return fmt.Errorf("refusing to fetch from non-public address %s", ip)
	}
	return nil
}

// isNonPublicIP reports whether ip is loopback, private (RFC1918 / IPv6 ULA),
// link-local, carrier-grade-NAT, or otherwise not a normal public destination.
func isNonPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsPrivate() {
		return true
	}
	// Carrier-grade NAT: 100.64.0.0/10.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return true
	}
	return false
}

func setUnfurlHeaders(req *http.Request) {
	req.Header.Set("User-Agent", unfurlUA)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
}

// unfurlLink fetches metadata for rawURL and returns a card, or nil if there's
// nothing worth showing. Runs on the sender's device only.
func unfurlLink(ctx context.Context, rawURL string) *peer.LinkCard {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil
	}
	client := unfurlClient()

	if id := youTubeID(rawURL); id != "" {
		card := &peer.LinkCard{URL: rawURL, Kind: "youtube", VideoID: id, SiteName: "YouTube"}
		title, thumb := youTubeMeta(ctx, client, id)
		card.Title = title
		if thumb != "" {
			card.Image = makeThumbFromURL(ctx, client, thumb, 480)
		}
		if card.Image == "" {
			card.Image = makeThumbFromURL(ctx, client, "https://i.ytimg.com/vi/"+id+"/hqdefault.jpg", 480)
		}
		return card
	}

	title, desc, image, site := fetchOG(ctx, client, rawURL)
	if title == "" && desc == "" && image == "" {
		return nil
	}
	card := &peer.LinkCard{URL: rawURL, Kind: "link", Title: title, Description: desc, SiteName: site}
	if image != "" {
		card.Image = makeThumbFromURL(ctx, client, image, 600)
	}
	if card.SiteName == "" {
		card.SiteName = u.Hostname()
	}
	return card
}

func sanitizeLinkCard(c peer.LinkCard) (peer.LinkCard, bool) {
	u, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return peer.LinkCard{}, false
	}
	c.URL = u.String()
	if c.Kind == "youtube" {
		if c.VideoID == "" {
			c.VideoID = youTubeID(c.URL)
		}
		c.VideoID = sanitizeYTID(c.VideoID)
		if c.VideoID == "" {
			c.Kind = "link"
		}
	} else {
		c.Kind = "link"
		c.VideoID = ""
	}
	c.Title = clean(c.Title, 200)
	c.Description = clean(c.Description, 400)
	c.SiteName = clean(c.SiteName, 80)
	c.Image = sanitizeCardImage(c.Image)
	return c, true
}

func sanitizeCardImage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 0 || len(s) > 128*1024 {
		return ""
	}
	prefixes := []string{
		"data:image/jpeg;base64,",
		"data:image/jpg;base64,",
		"data:image/png;base64,",
		"data:image/gif;base64,",
		"data:image/webp;base64,",
	}
	lower := strings.ToLower(s)
	ok := false
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			ok = true
			break
		}
	}
	if !ok {
		return ""
	}
	_, payload, found := strings.Cut(s, ",")
	if !found {
		return ""
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return ""
	}
	return s
}

// youTubeID extracts an 11-ish-char video id from the common YouTube URL
// shapes, or "" if rawURL isn't a YouTube video link.
func youTubeID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	switch host {
	case "youtu.be":
		return sanitizeYTID(strings.Trim(u.Path, "/"))
	case "youtube.com", "youtube-nocookie.com":
		if v := u.Query().Get("v"); v != "" {
			return sanitizeYTID(v)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 2 {
			switch parts[0] {
			case "shorts", "embed", "live", "v":
				return sanitizeYTID(parts[1])
			}
		}
	}
	return ""
}

func sanitizeYTID(s string) string {
	if i := strings.IndexAny(s, "?&/"); i >= 0 {
		s = s[:i]
	}
	if len(s) < 6 || len(s) > 20 {
		return ""
	}
	for _, r := range s {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return ""
	}
	return s
}

// youTubeMeta pulls a video's title + thumbnail via the public oEmbed endpoint
// (a sender-side fetch, like any other unfurl).
func youTubeMeta(ctx context.Context, client *http.Client, id string) (title, thumb string) {
	q := url.Values{}
	q.Set("format", "json")
	q.Set("url", "https://www.youtube.com/watch?v="+id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.youtube.com/oembed?"+q.Encode(), nil)
	if err != nil {
		return "", ""
	}
	setUnfurlHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var o struct {
		Title        string `json:"title"`
		ThumbnailURL string `json:"thumbnail_url"`
	}
	_ = json.Unmarshal(body, &o)
	return strings.TrimSpace(o.Title), o.ThumbnailURL
}

// fetchOG fetches rawURL and scrapes Open Graph / Twitter / <title> metadata.
func fetchOG(ctx context.Context, client *http.Client, rawURL string) (title, desc, image, site string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return
	}
	setUnfurlHeaders(req)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "html") {
		return
	}
	base := resp.Request.URL // final URL after redirects, for resolving relative images

	var htmlTitle string
	z := xhtml.NewTokenizer(io.LimitReader(resp.Body, 1<<20)) // 1 MB of HTML is plenty for <head>
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			break
		}
		if tt != xhtml.StartTagToken && tt != xhtml.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		tag := string(name)
		if tag == "body" {
			break // OG metadata lives in <head>; stop once content begins
		}
		switch tag {
		case "meta":
			var prop, nameAttr, content string
			for hasAttr {
				k, v, more := z.TagAttr()
				switch strings.ToLower(string(k)) {
				case "property":
					prop = string(v)
				case "name":
					nameAttr = string(v)
				case "content":
					content = string(v)
				}
				hasAttr = more
			}
			key := strings.ToLower(prop)
			if key == "" {
				key = strings.ToLower(nameAttr)
			}
			switch key {
			case "og:title", "twitter:title":
				if title == "" {
					title = content
				}
			case "og:description", "twitter:description", "description":
				if desc == "" {
					desc = content
				}
			case "og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src":
				if image == "" {
					image = content
				}
			case "og:site_name":
				if site == "" {
					site = content
				}
			}
		case "title":
			if z.Next() == xhtml.TextToken && htmlTitle == "" {
				htmlTitle = strings.TrimSpace(string(z.Text()))
			}
		}
	}

	if title == "" {
		title = htmlTitle
	}
	title = clean(stdhtml.UnescapeString(title), 200)
	desc = clean(stdhtml.UnescapeString(desc), 400)
	site = clean(stdhtml.UnescapeString(site), 80)
	if image != "" {
		image = stdhtml.UnescapeString(image)
		if iu, err := url.Parse(image); err == nil {
			image = base.ResolveReference(iu).String()
		}
	}
	return
}

// clean collapses whitespace and truncates to max runes.
func clean(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}

// makeThumbFromURL downloads an image and returns a downscaled JPEG data: URI,
// or "" on any failure (a missing thumbnail just yields a text-only card).
func makeThumbFromURL(ctx context.Context, client *http.Client, imageURL string, max int) string {
	u, err := url.Parse(imageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return ""
	}
	setUnfurlHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MB source cap
	if err != nil {
		return ""
	}
	if dataURI, ok := makeThumb(raw, max); ok {
		return dataURI
	}
	log.Printf("unfurl: thumbnail decode/resize failed for %s", imageURL)
	return ""
}

// maxDecodePixels caps an image's DECLARED dimensions before we hand its bytes
// to image.Decode (which allocates the full pixel buffer). 24 MP is generous for
// a real photo while rejecting decompression bombs (a tiny file that claims to
// be, say, 60000×60000).
const maxDecodePixels = 24 * 1000 * 1000

// decodableSize reads only the image header and reports whether its declared
// dimensions are small enough to decode without risking a memory blowup.
func decodableSize(raw []byte) bool {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return false
	}
	return int64(cfg.Width)*int64(cfg.Height) <= maxDecodePixels
}

// makeThumb decodes raw image bytes, downscales the longest side to max, and
// re-encodes a JPEG kept under ~70 KB (so the sealed MESSAGE_CARD stays well
// inside the 256 KB frame cap). Returns a data: URI and true on success.
func makeThumb(raw []byte, max int) (string, bool) {
	// Guard against decompression bombs: image.Decode allocates a buffer sized to
	// the image's DECLARED dimensions, so a tiny file claiming huge dimensions can
	// exhaust memory. Check the header first and bail before allocating.
	if !decodableSize(raw) {
		return "", false
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", false
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return "", false
	}
	nw, nh := w, h
	if w > max || h > max {
		if w >= h {
			nw, nh = max, h*max/w
		} else {
			nw, nh = w*max/h, max
		}
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	areaResize(dst, img)

	for _, q := range []int{82, 70, 55} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: q}); err != nil {
			return "", false
		}
		if buf.Len() <= 70*1024 || (q == 55 && buf.Len() <= 90*1024) {
			return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), true
		}
	}
	if max > 320 {
		return makeThumb(raw, 320) // still too big — shrink dimensions and retry
	}
	return "", false
}

// areaResize box-averages src into dst (any downscale ratio), compositing any
// transparency over white so transparent PNG logos don't turn into black
// boxes when flattened to JPEG.
func areaResize(dst *image.RGBA, src image.Image) {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dw, dh := dst.Bounds().Dx(), dst.Bounds().Dy()
	for dy := 0; dy < dh; dy++ {
		sy0 := sb.Min.Y + dy*sh/dh
		sy1 := sb.Min.Y + (dy+1)*sh/dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < dw; dx++ {
			sx0 := sb.Min.X + dx*sw/dw
			sx1 := sb.Min.X + (dx+1)*sw/dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rr, gg, bb, aa, n uint64
			for yy := sy0; yy < sy1; yy++ {
				for xx := sx0; xx < sx1; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					rr += uint64(cr >> 8)
					gg += uint64(cg >> 8)
					bb += uint64(cb >> 8)
					aa += uint64(ca >> 8)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			r, g, bl, al := rr/n, gg/n, bb/n, aa/n
			r = (r*al + 255*(255-al)) / 255
			g = (g*al + 255*(255-al)) / 255
			bl = (bl*al + 255*(255-al)) / 255
			dst.SetRGBA(dx, dy, color.RGBA{uint8(r), uint8(g), uint8(bl), 255})
		}
	}
}
