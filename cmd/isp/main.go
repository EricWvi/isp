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
	"isp/internal/health"
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
	if len(args) < 2 {
		return usage()
	}
	command := args[0]
	switch command {
	case "config-check", "state-init", "serve":
		if len(args) != 2 {
			return usage()
		}
	case "auto-switch":
		if len(args) != 3 || (args[2] != "on" && args[2] != "off") {
			return usage()
		}
	case "select":
		if len(args) != 4 {
			return usage()
		}
	default:
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
	if command == "auto-switch" {
		if err := router.SetAutoSwitch(context.Background(), args[2] == "on"); err != nil {
			return err
		}
		fmt.Println("automatic switch", args[2])
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	manager, err := health.New(ctx, cfg, store, router, nil)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Server.SOCKS5Listen)
	if err != nil {
		return err
	}
	log.Printf("SOCKS5 listening on %s", listener.Addr())
	server := &socks5.Server{Selector: router, OnUpstreamError: func(ref routing.Ref, err error) {
		log.Printf("upstream %s/%s: %v", ref.ProviderID, ref.ProxyID, err)
		if reportErr := manager.ReportConnectionError(ctx, ref); reportErr != nil {
			log.Printf("schedule health recheck for %s/%s: %v", ref.ProviderID, ref.ProxyID, reportErr)
		}
	}}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- manager.Run(serveCtx) }()
	go func() { results <- server.Serve(serveCtx, listener) }()
	first := <-results
	cancel()
	second := <-results
	if first != nil {
		return first
	}
	return second
}

func usage() error {
	return fmt.Errorf("usage: isp <config-check|state-init|serve> <config.yaml> | isp select <config.yaml> <provider-id> <proxy-id> | isp auto-switch <config.yaml> <on|off>")
}
