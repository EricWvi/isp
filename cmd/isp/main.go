package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"isp"
	"isp/internal/admin"
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
	file, err := config.Open(args[1])
	if err != nil {
		return err
	}
	cfg := file.Snapshot()
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
	static, err := isp.FrontendHandler()
	if err != nil {
		return fmt.Errorf("frontend build missing: %w", err)
	}
	listener, err := net.Listen("tcp", cfg.Server.SOCKS5Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	httpListener, err := net.Listen("tcp", cfg.Server.HTTPListen)
	if err != nil {
		return err
	}
	defer httpListener.Close()
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	app := &admin.App{Config: file, Router: router, Health: manager, Store: store, OnFatal: func(err error) {
		log.Printf("management update failed after YAML write: %v", err)
		cancel()
	}}
	mux := http.NewServeMux()
	mux.Handle("/api/", app.Handler())
	mux.Handle("/", static)
	httpServer := &http.Server{Handler: admin.LocalOnly(mux), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("SOCKS5 listening on %s", listener.Addr())
	log.Printf("management page listening on %s", httpListener.Addr())
	server := &socks5.Server{Selector: router, OnUpstreamError: func(ref routing.Ref, err error) {
		log.Printf("upstream %s/%s: %v", ref.ProviderID, ref.ProxyID, err)
		if reportErr := manager.ReportConnectionError(ctx, ref); reportErr != nil {
			log.Printf("schedule health recheck for %s/%s: %v", ref.ProviderID, ref.ProxyID, reportErr)
		}
	}}
	results := make(chan error, 3)
	go func() { results <- manager.Run(serveCtx) }()
	go func() { results <- server.Serve(serveCtx, listener) }()
	go func() {
		err := httpServer.Serve(httpListener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		results <- err
	}()
	go func() {
		<-serveCtx.Done()
		shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopShutdown()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	first := <-results
	cancel()
	second := <-results
	third := <-results
	if first != nil {
		return first
	}
	if second != nil {
		return second
	}
	return third
}

func usage() error {
	return fmt.Errorf("usage: isp <config-check|state-init|serve> <config.yaml> | isp select <config.yaml> <provider-id> <proxy-id> | isp auto-switch <config.yaml> <on|off>")
}
