// File-transfer ("flip") logic on top of the peer protocol.
//
// MVP shape:
//   - Sender opens the file, sends FLIP_START, streams ChunkSize-sized
//     FLIP_CHUNK envelopes, then FLIP_END with the sha256.
//   - Receiver creates the destination file in CatchRoot/<peer>/<filename>
//     (with name disambiguation), appends each chunk, verifies the hash on
//     FLIP_END, emits a flip-completed event.
//   - Errors at either end emit flip-failed.
//
// Deferred for later phases: resumability, pause/cancel/retry, multi-file
// flips, folder flips, ACK-based flow control, binary framing.
package peer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	mimepkg "mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// MaxFlipBytes is a hard ceiling on a single received file. Generous for
	// video, but bounds a malicious peer that declares a tiny size then streams
	// forever to fill the victim's disk.
	MaxFlipBytes = int64(4) << 30 // 4 GiB
	// MaxConcurrentInFlips caps simultaneous inbound transfers so a peer can't
	// open thousands of files/handles at once.
	MaxConcurrentInFlips = 24
)

type incomingFlip struct {
	id      string
	peer    string
	info    FlipStart
	file    *os.File
	path    string
	written int64
	hasher  hash.Hash
	mu      sync.Mutex
}

type outgoingFlip struct {
	id       string
	peer     string
	filename string
	mime     string
	size     int64
	path     string
	boothID  string
	cancel   chan struct{}
}

// SendFlip begins streaming localPath to peerName as a 1:1 (no booth scope).
// Returns the generated flip id once FLIP_START has been sent.
func (h *Hub) SendFlip(peerName, localPath string) (string, error) {
	id, err := newFlipID()
	if err != nil {
		return "", err
	}
	return id, h.SendFlipWithID(peerName, localPath, id, "")
}

// SendFlipWithID is like SendFlip but uses a caller-supplied id and a booth
// scope. Booth-flips share one id across recipients and carry the booth id so
// receivers file the result under the right conversation (passing "" sends it
// as a 1:1). The booth id keeps a file from surfacing in other rooms that
// happen to share a member.
func (h *Hub) SendFlipWithID(peerName, localPath, id, boothID string) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat %s: %w", localPath, err)
	}
	base := filepath.Base(localPath)
	mimeType := mimepkg.TypeByExtension(filepath.Ext(base))
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = strings.TrimSpace(mimeType[:i])
	}
	if mimeType == "" {
		// Unknown/missing extension (Windows' registry-backed lookup is also
		// spotty): sniff the content so images/video/PDF still preview, then
		// rewind so the actual transfer reads from the start.
		head := make([]byte, 512)
		if n, _ := f.Read(head); n > 0 {
			mimeType = http.DetectContentType(head[:n])
			if i := strings.IndexByte(mimeType, ';'); i >= 0 {
				mimeType = strings.TrimSpace(mimeType[:i])
			}
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return fmt.Errorf("rewind %s: %w", localPath, err)
		}
	}

	start := FlipStart{ID: id, Filename: base, Size: info.Size(), Mime: mimeType, BoothID: boothID}
	if err := c.WriteFrame(TypeFlipStart, start); err != nil {
		f.Close()
		return fmt.Errorf("send FLIP_START: %w", err)
	}

	out := &outgoingFlip{
		id:       id,
		peer:     peerName,
		filename: base,
		mime:     mimeType,
		size:     info.Size(),
		path:     localPath,
		boothID:  boothID,
		cancel:   make(chan struct{}),
	}
	h.flipMu.Lock()
	h.outFlips[id] = out
	h.flipMu.Unlock()

	h.emit(HubEvent{
		Kind: EventFlipStarted,
		Peer: peerName,
		Text: "outbound " + base,
		Data: &FlipEventData{
			ID:        id,
			Direction: "out",
			Filename:  base,
			Size:      info.Size(),
			Mime:      mimeType,
			Path:      localPath,
			BoothID:   boothID,
		},
	})

	go h.runOutgoingFlip(c, f, out, start)
	return nil
}

func (h *Hub) runOutgoingFlip(c *PeerConn, f *os.File, out *outgoingFlip, start FlipStart) {
	defer f.Close()
	defer func() {
		h.flipMu.Lock()
		delete(h.outFlips, out.id)
		h.flipMu.Unlock()
	}()

	hasher := sha256.New()
	buf := make([]byte, ChunkSize)
	var offset int64
	for {
		select {
		case <-out.cancel:
			h.emit(HubEvent{Kind: EventFlipFailed, Peer: c.Name, Text: "cancelled",
				Data: &FlipEventData{ID: out.id, Direction: "out", Filename: out.filename, Mime: out.mime, Size: out.size, Path: out.path, BoothID: out.boothID, Bytes: offset, Reason: "cancelled"}})
			return
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			chunk := FlipChunk{ID: out.id, Offset: offset, Data: buf[:n]}
			if werr := c.WriteFrame(TypeFlipChunk, chunk); werr != nil {
				h.emitFlipFailed(c.Name, out, offset, "send chunk: "+werr.Error())
				return
			}
			hasher.Write(buf[:n])
			offset += int64(n)
			h.emit(HubEvent{Kind: EventFlipProgress, Peer: c.Name, Text: out.filename,
				Data: &FlipEventData{ID: out.id, Direction: "out", Filename: out.filename, Mime: out.mime, Size: out.size, Path: out.path, BoothID: out.boothID, Bytes: offset}})
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			h.emitFlipFailed(c.Name, out, offset, "read: "+err.Error())
			return
		}
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	if err := c.WriteFrame(TypeFlipEnd, FlipEnd{ID: out.id, Sha256: sum}); err != nil {
		h.emitFlipFailed(c.Name, out, offset, "send END: "+err.Error())
		return
	}
	h.emit(HubEvent{Kind: EventFlipCompleted, Peer: c.Name, Text: out.filename,
		Data: &FlipEventData{ID: out.id, Direction: "out", Filename: out.filename, Mime: out.mime, Size: out.size, Path: out.path, BoothID: out.boothID, Bytes: offset, Sha256: sum}})
}

func (h *Hub) emitFlipFailed(peerName string, out *outgoingFlip, sent int64, reason string) {
	h.emit(HubEvent{Kind: EventFlipFailed, Peer: peerName, Text: reason,
		Data: &FlipEventData{ID: out.id, Direction: "out", Filename: out.filename, Size: out.size, Path: out.path, BoothID: out.boothID, Bytes: sent, Reason: reason}})
}

func (h *Hub) handleFlipStart(peerName string, start FlipStart) {
	if h.CatchRoot == "" {
		h.sendReject(peerName, start.ID, "catch directory not configured")
		return
	}
	if start.Size < 0 || start.Size > MaxFlipBytes {
		h.sendReject(peerName, start.ID, "file too large")
		return
	}
	h.flipMu.Lock()
	tooMany := len(h.inFlips) >= MaxConcurrentInFlips
	h.flipMu.Unlock()
	if tooMany {
		h.sendReject(peerName, start.ID, "too many concurrent transfers")
		return
	}
	base := safeBasename(start.Filename)
	if base == "" {
		h.sendReject(peerName, start.ID, "invalid filename")
		return
	}
	peerDir := filepath.Join(h.CatchRoot, sanitizeForPath(peerName))
	if err := os.MkdirAll(peerDir, 0o700); err != nil {
		h.sendReject(peerName, start.ID, "mkdir catch: "+err.Error())
		return
	}
	dest := uniquePath(filepath.Join(peerDir, base))
	f, err := os.Create(dest)
	if err != nil {
		h.sendReject(peerName, start.ID, "create file: "+err.Error())
		return
	}
	in := &incomingFlip{
		id:     start.ID,
		peer:   peerName,
		info:   start,
		file:   f,
		path:   dest,
		hasher: sha256.New(),
	}
	h.flipMu.Lock()
	h.inFlips[start.ID] = in
	h.flipMu.Unlock()
	h.emit(HubEvent{
		Kind: EventFlipStarted,
		Peer: peerName,
		Text: "inbound " + base,
		Data: &FlipEventData{
			ID:        start.ID,
			Direction: "in",
			Filename:  base,
			Size:      start.Size,
			Mime:      start.Mime,
			Path:      dest,
			BoothID:   start.BoothID,
		},
	})
}

func (h *Hub) handleFlipChunk(peerName string, chunk FlipChunk) {
	h.flipMu.Lock()
	in, ok := h.inFlips[chunk.ID]
	h.flipMu.Unlock()
	if !ok {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	// Reject a sender that streams more than the size it declared (or past the
	// hard cap): otherwise a malicious peer could declare a tiny file and fill
	// the victim's disk.
	if in.written+int64(len(chunk.Data)) > in.info.Size || in.written+int64(len(chunk.Data)) > MaxFlipBytes {
		in.file.Close()
		os.Remove(in.path)
		h.flipMu.Lock()
		delete(h.inFlips, chunk.ID)
		h.flipMu.Unlock()
		h.emit(HubEvent{Kind: EventFlipFailed, Peer: peerName, Text: "oversized transfer",
			Data: &FlipEventData{ID: in.id, Direction: "in", Filename: in.info.Filename, Size: in.info.Size, Path: in.path, BoothID: in.info.BoothID, Bytes: in.written, Reason: "sender exceeded declared size"}})
		return
	}
	if _, err := in.file.Write(chunk.Data); err != nil {
		h.emit(HubEvent{Kind: EventFlipFailed, Peer: peerName, Text: "write: " + err.Error(),
			Data: &FlipEventData{ID: in.id, Direction: "in", Filename: in.info.Filename, Size: in.info.Size, Path: in.path, BoothID: in.info.BoothID, Bytes: in.written, Reason: err.Error()}})
		in.file.Close()
		h.flipMu.Lock()
		delete(h.inFlips, chunk.ID)
		h.flipMu.Unlock()
		return
	}
	in.hasher.Write(chunk.Data)
	in.written += int64(len(chunk.Data))
	h.emit(HubEvent{Kind: EventFlipProgress, Peer: peerName, Text: in.info.Filename,
		Data: &FlipEventData{ID: in.id, Direction: "in", Filename: in.info.Filename, Size: in.info.Size, Path: in.path, Bytes: in.written, Mime: in.info.Mime, BoothID: in.info.BoothID}})
}

func (h *Hub) handleFlipEnd(peerName string, end FlipEnd) {
	h.flipMu.Lock()
	in, ok := h.inFlips[end.ID]
	delete(h.inFlips, end.ID)
	h.flipMu.Unlock()
	if !ok {
		return
	}
	in.file.Close()
	got := hex.EncodeToString(in.hasher.Sum(nil))
	if got != end.Sha256 {
		os.Remove(in.path)
		h.emit(HubEvent{Kind: EventFlipFailed, Peer: peerName, Text: "sha256 mismatch",
			Data: &FlipEventData{ID: in.id, Direction: "in", Filename: in.info.Filename, Size: in.info.Size, Path: in.path, BoothID: in.info.BoothID, Bytes: in.written, Reason: "sha256 mismatch"}})
		return
	}
	h.emit(HubEvent{Kind: EventFlipCompleted, Peer: peerName, Text: in.info.Filename,
		Data: &FlipEventData{ID: in.id, Direction: "in", Filename: in.info.Filename, Size: in.info.Size, Mime: in.info.Mime, Path: in.path, BoothID: in.info.BoothID, Bytes: in.written, Sha256: got}})
}

func (h *Hub) handleFlipReject(peerName string, r FlipReject) {
	h.flipMu.Lock()
	out, ok := h.outFlips[r.ID]
	delete(h.outFlips, r.ID)
	h.flipMu.Unlock()
	if !ok {
		return
	}
	close(out.cancel)
	h.emit(HubEvent{Kind: EventFlipFailed, Peer: peerName, Text: "rejected: " + r.Reason,
		Data: &FlipEventData{ID: r.ID, Direction: "out", Filename: out.filename, Size: out.size, Path: out.path, Reason: "rejected: " + r.Reason}})
}

func (h *Hub) sendReject(peerName, id, reason string) {
	c := h.Get(peerName)
	if c == nil {
		return
	}
	_ = c.WriteFrame(TypeFlipReject, FlipReject{ID: id, Reason: reason})
}

// ---------- helpers ----------

func newFlipID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // RFC 4122 v4
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func safeBasename(name string) string {
	name = filepath.Base(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	// Replace path separators AND the rest of the Windows-reserved set. ':' is
	// the important one: a name like "report.pdf:evil.exe" would otherwise create
	// a hidden NTFS alternate data stream instead of a visible file.
	for _, c := range []string{"\\", "/", ":", "*", "?", "\"", "<", ">", "|"} {
		name = strings.ReplaceAll(name, c, "_")
	}
	// Neutralize control characters (incl. NUL) that could confuse the OS or the
	// UI that later renders the name.
	name = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return '_'
		}
		return r
	}, name)
	// Windows silently strips trailing dots/spaces, so "evil.exe . " resolves to
	// "evil.exe": trim them ourselves so the on-disk name matches what the user
	// sees and can't smuggle a hidden trailing extension.
	name = strings.TrimRight(name, " .")
	// Reserved DOS device names (CON, NUL, COM1…) are special at any path depth.
	if isReservedDeviceName(name) {
		name = "_" + name
	}
	if name == "" {
		return ""
	}
	// Bound the length so a pathological name can't blow past OS path limits.
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:200-len(ext)] + ext
	}
	return name
}

// isReservedDeviceName reports whether name (ignoring any extension) is a
// Windows reserved device name, which the OS treats as a device regardless of
// the directory it's in.
func isReservedDeviceName(name string) bool {
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	switch strings.ToUpper(stem) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

func sanitizeForPath(name string) string {
	for _, c := range []string{"\\", "/", ":", "*", "?", "\"", "<", ">", "|"} {
		name = strings.ReplaceAll(name, c, "_")
	}
	return name
}

// uniquePath returns p if it doesn't exist, otherwise p with " (1)", " (2)", ...
// inserted before the extension until a free name is found.
func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}
