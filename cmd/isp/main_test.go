package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"isp/internal/config"
	"isp/internal/state"
)

func TestSelectCommandPersistsChoice(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Server.Database = filepath.Join(dir, "isp.db")
	cfg.Providers = []config.Provider{{ID: "seller", Type: "proxy-seller", Enabled: true, Proxies: []config.Proxy{{ID: "one", Host: "192.0.2.1", Port: 1080, Enabled: true}}}}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"select", path, "seller", "one"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"auto-switch", path, "on"}); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(context.Background(), cfg.Server.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	selection, err := store.LoadSelection(context.Background())
	if err != nil || selection.ProviderID != "seller" || selection.ProxyID != "one" || !selection.AutoSwitch {
		t.Fatalf("selection: %+v, %v", selection, err)
	}
}

func TestServeProcess(t *testing.T) {
	path := os.Getenv("ISP_TEST_SERVE_CONFIG")
	if path == "" {
		return
	}
	if err := run([]string{"serve", path}); err != nil {
		t.Fatal(err)
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Server.Database = filepath.Join(dir, "isp.db")
	for _, address := range []*string{&cfg.Server.HTTPListen, &cfg.Server.SOCKS5Listen} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		*address = listener.Addr().String()
		listener.Close()
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeProcess$")
	cmd.Env = append(os.Environ(), "ISP_TEST_SERVE_CONFIG="+path)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	client := http.Client{Timeout: 200 * time.Millisecond}
	ready := false
	for !ready && ctx.Err() == nil {
		response, err := client.Get("http://" + cfg.Server.HTTPListen + "/api/state")
		if err == nil {
			response.Body.Close()
			ready = response.StatusCode == http.StatusOK
		}
		if !ready {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("service did not start: %s", output.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("service did not stop cleanly: %v\n%s", err, output.String())
	}
	if !bytes.Contains(output.Bytes(), []byte(`"msg":"service stopped"`)) {
		t.Fatalf("missing structured shutdown log: %s", output.String())
	}
	store, err := state.Open(context.Background(), cfg.Server.Database)
	if err != nil {
		t.Fatalf("database could not be reopened after shutdown: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
