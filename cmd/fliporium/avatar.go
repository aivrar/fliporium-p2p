package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"os"

	"fliporium/internal/peer"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// settingSelfAvatar is the app_settings key holding our own avatar data URI.
const settingSelfAvatar = "self_avatar"

// selfAvatar returns our stored avatar data URI ("" if none / store not ready).
func (a *App) selfAvatar() string {
	if a.store == nil {
		return ""
	}
	v, _ := a.store.GetSetting(a.ctx, settingSelfAvatar)
	return v
}

// PickAvatar opens a file picker, downscales the chosen image to a small square
// avatar, stores it as ours, announces it to peers (they pick it up on their
// next reconnect, like the display name), and returns the data URI so the UI
// can show it immediately. Returns "" if the user cancelled.
func (a *App) PickAvatar() (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("store not ready")
	}
	path, err := wailsruntime.OpenFileDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Choose a profile picture",
		Filters: []wailsruntime.FileFilter{
			{DisplayName: "Images (png, jpg, gif)", Pattern: "*.png;*.jpg;*.jpeg;*.gif"},
		},
	})
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil // cancelled
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read image: %w", err)
	}
	uri, ok := avatarDataURI(raw)
	if !ok {
		return "", fmt.Errorf("that file isn't a supported image (use png, jpg, or gif)")
	}
	return uri, a.setAvatar(uri)
}

// ClearAvatar removes our avatar (falls back to initials everywhere).
func (a *App) ClearAvatar() error { return a.setAvatar("") }

// setAvatar persists the avatar (or clears it when uri==""), pushes it to every
// live hub so it rides the next HELLO, updates self, and notifies the UI.
func (a *App) setAvatar(uri string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	if uri == "" {
		_ = a.store.DeleteSetting(a.ctx, settingSelfAvatar)
	} else if err := a.store.SetSetting(a.ctx, settingSelfAvatar, uri); err != nil {
		return err
	}
	a.eachHub(func(h *peer.Hub) { h.SetSelfAvatar(uri) })
	a.mu.Lock()
	a.self.Avatar = uri
	a.mu.Unlock()
	if a.ctx != nil {
		wailsruntime.EventsEmit(a.ctx, "app-state", a.Status())
	}
	return nil
}

// avatarDataURI center-crops raw image bytes to a square, downscales to a small
// fixed size, and returns a compact JPEG data: URI. ok=false if the bytes
// aren't a decodable image. Kept small because the result rides every HELLO.
func avatarDataURI(raw []byte) (string, bool) {
	// Reject decompression bombs before image.Decode allocates the pixel buffer
	// (decodableSize lives in unfurl.go, same package).
	if !decodableSize(raw) {
		return "", false
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", false
	}
	const side = 96
	dst := image.NewRGBA(image.Rect(0, 0, side, side))
	areaResize(dst, cropSquare(img)) // box-average downscaler from unfurl.go
	for _, q := range []int{80, 65, 50} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: q}); err != nil {
			return "", false
		}
		if buf.Len() <= 16*1024 || q == 50 {
			return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), true
		}
	}
	return "", false
}

// cropSquare returns the largest centered square sub-image of img.
func cropSquare(img image.Image) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	s := w
	if h < s {
		s = h
	}
	x0 := b.Min.X + (w-s)/2
	y0 := b.Min.Y + (h-s)/2
	rect := image.Rect(x0, y0, x0+s, y0+s)
	if sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return sub.SubImage(rect)
	}
	// Fallback for image types without SubImage (std decoders all have it).
	dst := image.NewRGBA(image.Rect(0, 0, s, s))
	draw.Draw(dst, dst.Bounds(), img, rect.Min, draw.Src)
	return dst
}
