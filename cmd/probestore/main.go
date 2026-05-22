// probestore dumps the contents of a fliporium store.db for verification.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fliporium/internal/store"

	_ "modernc.org/sqlite"
)

func main() {
	dir := "./fliporium-data"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	s, err := store.Open(dir)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer s.Close()

	ctx := context.Background()

	// Peer roster (only the GUI populates this; CLI leaves it empty)
	peers, _ := s.Peers(ctx)
	fmt.Printf("== peers (%d) ==\n", len(peers))
	for _, p := range peers {
		fmt.Printf("  %-20s display=%q first=%s  last=%s\n", p.Name, p.Display, p.FirstSeen.Format("15:04:05Z"), p.LastSeen.Format("15:04:05Z"))
	}

	// Twin setting
	if v, _ := s.GetSetting(ctx, store.SettingTwinHostname); v != "" {
		fmt.Printf("\n== twin == %s\n", v)
	}

	// Distinct senders/recipients across all 1:1 messages (bypassing the
	// peer table — works even when the CLI never wrote peer rows).
	names := distinctMessagePeers(dir)
	if len(names) > 0 {
		fmt.Println()
		for _, name := range names {
			msgs, _ := s.Messages(ctx, name, 100)
			if len(msgs) == 0 {
				continue
			}
			fmt.Printf("== messages with %s (%d) ==\n", name, len(msgs))
			for _, m := range msgs {
				arrow := "<-"
				if m.Direction == store.DirectionOut {
					arrow = "->"
				}
				fmt.Printf("  %s  %s  %s  %s\n", m.At.Format("15:04:05"), arrow, m.Peer, m.Text)
			}
		}
	}

	// Per-peer flips
	for _, p := range peers {
		flips, _ := s.FlipsByPeer(ctx, p.Name)
		if len(flips) == 0 {
			continue
		}
		fmt.Printf("== flips with %s (%d) ==\n", p.Name, len(flips))
		for _, f := range flips {
			arrow := "<-"
			if f.Direction == store.DirectionOut {
				arrow = "->"
			}
			fmt.Printf("  %s  %s  [%s]  %s  %d bytes  status=%s  mime=%s  path=%s\n",
				f.StartedAt.Format("15:04:05"), arrow, f.ID[:8], f.Filename, f.Size, f.Status, f.Mime, f.Path)
		}
	}

	// Booths
	booths, _ := s.ListBooths(ctx)
	fmt.Println()
	fmt.Printf("== booths (%d) ==\n", len(booths))
	for _, b := range booths {
		members, _ := s.BoothMembers(ctx, b.ID)
		mnames := make([]string, 0, len(members))
		for _, m := range members {
			mnames = append(mnames, m.PeerName)
		}
		fmt.Printf("  [%s]  %q  founder=%s  members=[%s]\n", b.ID[:8], b.Name, b.Founder, strings.Join(mnames, ", "))

		msgs, _ := s.MessagesByBooth(ctx, b.ID, 100)
		if len(msgs) > 0 {
			fmt.Printf("    %d messages:\n", len(msgs))
			for _, m := range msgs {
				arrow := "<-"
				if m.Direction == store.DirectionOut {
					arrow = "->"
				}
				fmt.Printf("    %s  %s  %s  %s\n", m.At.Format("15:04:05"), arrow, m.Peer, m.Text)
			}
		}

		n, _ := s.GetBoothNotepad(ctx, b.ID)
		if n.Version > 0 {
			preview := n.Text
			if len(preview) > 80 {
				preview = preview[:80] + "..."
			}
			fmt.Printf("    notepad: v%d by %s at %s: %s\n", n.Version, n.LastEditor, n.LastModified.Format("15:04:05"), preview)
		}
	}
	_ = time.Now // keep import
}

// distinctMessagePeers opens the same store.db with a raw sqlite handle to
// run a quick SELECT DISTINCT — the store package doesn't expose this query.
func distinctMessagePeers(dir string) []string {
	path := filepath.Join(dir, "store.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT DISTINCT peer FROM messages WHERE booth_id IS NULL ORDER BY peer`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
