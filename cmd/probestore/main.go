// probestore dumps the contents of a fliporium store.db for verification.
package main

import (
	"context"
	"fmt"
	"os"

	"fliporium/internal/store"
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

	peers, err := s.Peers(ctx)
	if err != nil {
		fmt.Println("peers:", err)
	}
	fmt.Printf("== peers (%d) ==\n", len(peers))
	for _, p := range peers {
		fmt.Printf("  %-20s first=%s  last=%s\n", p.Name, p.FirstSeen.Format("15:04:05Z"), p.LastSeen.Format("15:04:05Z"))
	}

	fmt.Println()
	for _, p := range peers {
		msgs, _ := s.Messages(ctx, p.Name, 100)
		fmt.Printf("== messages with %s (%d) ==\n", p.Name, len(msgs))
		for _, m := range msgs {
			arrow := "<-"
			if m.Direction == "out" {
				arrow = "->"
			}
			fmt.Printf("  %s  %s  %s  %s\n", m.At.Format("15:04:05"), arrow, m.Peer, m.Text)
		}

		flips, _ := s.FlipsByPeer(ctx, p.Name)
		fmt.Printf("== flips with %s (%d) ==\n", p.Name, len(flips))
		for _, f := range flips {
			arrow := "<-"
			if f.Direction == "out" {
				arrow = "->"
			}
			fmt.Printf("  %s  %s  [%s]  %s  %d bytes  status=%s  mime=%s  path=%s\n",
				f.StartedAt.Format("15:04:05"), arrow, f.ID[:8], f.Filename, f.Size, f.Status, f.Mime, f.Path)
		}
	}
}
