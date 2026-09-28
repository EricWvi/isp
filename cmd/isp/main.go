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
	"isp/internal/httpproxy"
	"isp/internal/routing"
	"isp/internal/socks5"
	"isp/internal/state"
)

// version is replaced at build time with -ldflags "-X main.version=<git tag>".
// It must stay a variable because the linker cannot rewrite constants.
var version = "unknown"

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) (runErr error) {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println(version)
		return nil
	}
	if len(args) < 2 {
		return usage()
	}
	command := args[0]
	switch command {
	case "config-check", "state-init", "serve":
		if len(args) != 2 {
			return usage()
		}
	case "auto-switch", "auto-switch-http":
		if len(args) != 3 || (args[2] != "on" && args[2] != "off") {
			return usage()
		}
	case "select", "select-http":
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
	router, err := routing.Open(context.Background(), cfg.ForProtocol("socks5"), store)
	if err != nil {
		return err
	}
	httpRouter, err := routing.Open(context.Background(), cfg.ForProtocol("http"), store.HTTPSelectionStore())
	if err != nil {
		return err
	}
	if command == "select" || command == "select-http" {
		selectedRouter := router
		if command == "select-http" {
			selectedRouter = httpRouter
		}
		if err := selectedRouter.Select(context.Background(), routing.Ref{ProviderID: args[2], ProxyID: args[3]}); err != nil {
			return err
		}
		fmt.Println("current proxy selected")
		return nil
	}
	if command == "auto-switch" || command == "auto-switch-http" {
		selectedRouter := router
		if command == "auto-switch-http" {
			selectedRouter = httpRouter
		}
		if err := selectedRouter.SetAutoSwitch(context.Background(), args[2] == "on"); err != nil {
			return err
		}
		fmt.Println("automatic switch", args[2])
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	manager, err := health.New(ctx, cfg.ForProtocol("socks5"), store, router, nil)
	if err != nil {
		return err
	}
	httpManager, err := health.New(ctx, cfg.ForProtocol("http"), store, httpRouter, health.HTTPUpstreamProbe)
	if err != nil {
		return err
	}
	static, err := isp.FrontendHandler()
	if err != nil {
		return fmt.Errorf("frontend build missing: %w", err)
	}
	var socksListener, proxyListener net.Listener
	if cfg.Server.SOCKS5Enabled {
		socksListener, err = net.Listen("tcp", cfg.Server.SOCKS5Listen)
		if err != nil {
			return fmt.Errorf("listen SOCKS5: %w", err)
		}
		defer socksListener.Close()
	}
	if cfg.Server.HTTPProxyEnabled {
		proxyListener, err = net.Listen("tcp", cfg.Server.HTTPProxyListen)
		if err != nil {
			return fmt.Errorf("listen HTTP proxy: %w", err)
		}
		defer proxyListener.Close()
	}
	httpListener, err := net.Listen("tcp", cfg.Server.HTTPListen)
	if err != nil {
		return fmt.Errorf("listen management page: %w", err)
	}
	defer httpListener.Close()
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	manager.OnError = func(err error) { logger.Error("SOCKS5 health state update failed", "error", err) }
	httpManager.OnError = func(err error) { logger.Error("HTTP health state update failed", "error", err) }
	fatal := make(chan error, 1)
	app := &admin.App{Config: file, Router: router, HTTPRouter: httpRouter, Health: manager, HTTPHealth: httpManager, Store: store, Version: version, OnFatal: func(err error) {
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
	if socksListener != nil {
		logger.Info("SOCKS5 listening", "address", socksListener.Addr().String())
	}
	if proxyListener != nil {
		logger.Info("HTTP proxy listening", "address", proxyListener.Addr().String())
	}
	logger.Info("management page listening", "address", httpListener.Addr().String())
	reportUpstreamError := func(manager *health.Manager) func(routing.Ref, error) {
		return func(ref routing.Ref, err error) {
			logger.Warn("upstream connection failed", "provider_id", ref.ProviderID, "proxy_id", ref.ProxyID, "error", err)
			if reportErr := manager.ReportConnectionError(ctx, ref); reportErr != nil {
				logger.Error("schedule health recheck failed", "provider_id", ref.ProviderID, "proxy_id", ref.ProxyID, "error", reportErr)
			}
		}
	}
	server := &socks5.Server{Selector: router, OnUpstreamError: reportUpstreamError(manager)}
	proxyHandler := &httpproxy.Handler{Selector: httpRouter, OnUpstreamError: reportUpstreamError(httpManager)}
	proxyServer := proxyHandler.Server()
	app.UDPCapability = server
	workers := 3
	if socksListener != nil {
		workers++
	}
	if proxyListener != nil {
		workers++
	}
	results := make(chan error, workers)
	go func() { results <- manager.Run(serveCtx) }()
	go func() { results <- httpManager.Run(serveCtx) }()
	if socksListener != nil {
		go func() { results <- server.Serve(serveCtx, socksListener) }()
	}
	if proxyListener != nil {
		go func() {
			err := proxyServer.Serve(proxyListener)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			results <- err
		}()
	}
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
		proxyHandler.Close()
		shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopShutdown()
		proxyErr := error(nil)
		if proxyListener != nil {
			proxyErr = proxyServer.Shutdown(shutdownCtx)
			if proxyErr != nil {
				_ = proxyServer.Close()
			}
		}
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			shutdownDone <- errors.Join(fmt.Errorf("shutdown management server: %w", err), proxyErr)
			return
		}
		shutdownDone <- proxyErr
	}()
	first := <-results
	cancel()
	runErr = first
	for i := 1; i < workers; i++ {
		runErr = errors.Join(runErr, <-results)
	}
	runErr = errors.Join(runErr, <-shutdownDone)
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
	return fmt.Errorf("usage: isp --version | isp <config-check|state-init|serve> <config.yaml> | isp <select|select-http> <config.yaml> <provider-id> <proxy-id> | isp <auto-switch|auto-switch-http> <config.yaml> <on|off>")
}
