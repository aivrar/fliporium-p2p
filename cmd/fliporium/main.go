// fliporium is the Phase 4 Wails desktop binary: a real Windows window
// (WebView2 under the hood) showing The Floor, the peer list, and a 1:1
// chat pane with SQLite-backed history and basic Markdown rendering.
//
// Configuration is the same as the CLI (FLIPORIUM_AUTHKEY, _HOSTNAME, _DIR).
package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailswindows "github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

// defaultDataDir returns <directory-containing-the-exe>/fliporium-data.
// This keeps the install portable: copy the .exe + fliporium-data folder to a
// USB stick, run from any working directory, and identity + history travel
// with the binary instead of getting orphaned in whatever CWD the shell was
// in at launch.
func defaultDataDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "fliporium-data")
	}
	return "fliporium-data"
}

func main() {
	// Windows GUI apps lose stdout/stderr by default. Route logs to a file
	// next to the data dir so init/runtime errors are recoverable.
	dir := os.Getenv("FLIPORIUM_DIR")
	if dir == "" {
		dir = defaultDataDir()
	}
	_ = os.MkdirAll(dir, 0o700)
	if f, err := os.OpenFile(filepath.Join(dir, "fliporium.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		log.SetOutput(f)
	}
	log.Printf("main: starting (hostname=%q, dir=%q)", os.Getenv("FLIPORIUM_HOSTNAME"), dir)

	// Wails expects index.html at the FS root, but //go:embed preserves
	// the source path. fs.Sub strips frontend/dist/ so the asset server
	// sees the files at the root.
	distFS, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		log.Fatalf("main: fs.Sub frontend/dist: %v", err)
	}
	if entries, err := fs.ReadDir(distFS, "."); err == nil {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		log.Printf("main: distFS root entries: %v", names)
	} else {
		log.Printf("main: distFS root read err: %v", err)
	}

	app := NewApp()

	err = wails.Run(&options.App{
		Title:     "Fliporium",
		Width:     1100,
		Height:    750,
		MinWidth:  720,
		MinHeight: 480,
		AssetServer: &assetserver.Options{
			Assets:  distFS,
			Handler: http.HandlerFunc(app.catchHandler),
		},
		BackgroundColour: &options.RGBA{R: 18, G: 18, B: 22, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Logger:           logger.NewFileLogger(filepath.Join(dir, "wails.log")),
		LogLevel:         logger.DEBUG,
		LogLevelProduction: logger.DEBUG,
		Bind: []interface{}{
			app,
		},
		Windows: &wailswindows.Options{
			WebviewUserDataPath: filepath.Join(dir, "webview"),
			Theme:               wailswindows.SystemDefault,
		},
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop: true,
		},
	})
	log.Printf("main: wails.Run returned err=%v, exiting", err)
}
