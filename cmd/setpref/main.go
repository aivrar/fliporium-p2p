// setpref is a one-shot helper to read/write the app_settings kv table.
package main

import (
	"context"
	"fmt"
	"os"

	"fliporium/internal/store"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: setpref <dir> <key> [<value>]")
		fmt.Println("  no value -> read; empty string -> delete")
		os.Exit(2)
	}
	s, err := store.Open(os.Args[1])
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer s.Close()
	ctx := context.Background()
	if len(os.Args) == 3 {
		v, _ := s.GetSetting(ctx, os.Args[2])
		fmt.Println(v)
		return
	}
	if os.Args[3] == "" {
		_ = s.DeleteSetting(ctx, os.Args[2])
		fmt.Println("deleted")
		return
	}
	_ = s.SetSetting(ctx, os.Args[2], os.Args[3])
	fmt.Println("set")
}
