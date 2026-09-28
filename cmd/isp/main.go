package main

import (
	"context"
	"fmt"
	"os"

	"isp/internal/config"
	"isp/internal/state"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 || (args[0] != "config-check" && args[0] != "state-init") {
		return fmt.Errorf("usage: isp <config-check|state-init> <config.yaml>")
	}
	cfg, err := config.Load(args[1])
	if err != nil {
		return err
	}
	if args[0] == "config-check" {
		fmt.Println("configuration valid")
		return nil
	}
	store, err := state.Open(context.Background(), cfg.Server.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	fmt.Println("state database ready")
	return nil
}
