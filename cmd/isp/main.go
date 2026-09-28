package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) (runErr error) {
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
	defer func() {
		if err := store.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close state database: %w", err))
		}
	}()
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
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	fatal := make(chan error, 1)
	app := &admin.App{Config: file, Router: router, Health: manager, Store: store, OnFatal: func(err error) {
		logger.Error("management update failed after YAML write", "error", err)
		select {
		case fatal <- err:
		default:
		}
		cancel()
	}}
	mux := http.NewServeMux()
	mux.Handle("/api/", app.Handler())
	mux.Handle("/", static)
	httpServer := &http.Server{Handler: admin.LocalOnly(mux), ReadHeaderTimeout: 5 * time.Second}
	logger.Info("SOCKS5 listening", "address", listener.Addr().String())
	logger.Info("management page listening", "address", httpListener.Addr().String())
	server := &socks5.Server{Selector: router, OnUpstreamError: func(ref routing.Ref, err error) {
		logger.Warn("upstream connection failed", "provider_id", ref.ProviderID, "proxy_id", ref.ProxyID, "error", err)
		if reportErr := manager.ReportConnectionError(ctx, ref); reportErr != nil {
			logger.Error("schedule health recheck failed", "provider_id", ref.ProviderID, "proxy_id", ref.ProxyID, "error", reportErr)
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
	shutdownDone := make(chan error, 1)
	go func() {
		<-serveCtx.Done()
		shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopShutdown()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			shutdownDone <- fmt.Errorf("shutdown management server: %w", err)
			return
		}
		shutdownDone <- nil
	}()
	first := <-results
	cancel()
	runErr = errors.Join(first, <-results, <-results, <-shutdownDone)
	select {
	case err := <-fatal:
		runErr = errors.Join(runErr, fmt.Errorf("management state diverged from YAML: %w", err))
	default:
	}
	if runErr == nil {
		logger.Info("service stopped")
	}
	return runErr
}

func usage() error {
	return fmt.Errorf("usage: isp <config-check|state-init|serve> <config.yaml> | isp select <config.yaml> <provider-id> <proxy-id> | isp auto-switch <config.yaml> <on|off>")
}
