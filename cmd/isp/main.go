package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"isp/internal/config"
	"isp/internal/routing"
	"isp/internal/socks5"
	"isp/internal/state"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 || len(args) > 4 {
		return usage()
	}
	command := args[0]
	if (command == "select" && len(args) != 4) || (command != "select" && len(args) != 2) {
		return usage()
	}
	cfg, err := config.Load(args[1])
	if err != nil {
		return err
	}
	if command == "config-check" {
		fmt.Println("configuration valid")
		return nil
	}
	if command != "state-init" && command != "select" && command != "serve" {
		return usage()
	}
	store, err := state.Open(context.Background(), cfg.Server.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	if command == "state-init" {
		fmt.Println("state database ready")
		return nil
	}
	router, err := routing.Open(context.Background(), cfg, store)
	if err != nil {
		return err
	}
	if command == "select" {
		if err := router.Select(context.Background(), routing.Ref{ProviderID: args[2], ProxyID: args[3]}); err != nil {
			return err
		}
		fmt.Println("current proxy selected")
		return nil
	}
	listener, err := net.Listen("tcp", cfg.Server.SOCKS5Listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("SOCKS5 listening on %s", listener.Addr())
	server := &socks5.Server{Selector: router, OnUpstreamError: func(ref routing.Ref, err error) {
		log.Printf("upstream %s/%s: %v", ref.ProviderID, ref.ProxyID, err)
	}}
	return server.Serve(ctx, listener)
}

func usage() error {
	return fmt.Errorf("usage: isp <config-check|state-init|serve> <config.yaml> | isp select <config.yaml> <provider-id> <proxy-id>")
}
