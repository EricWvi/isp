package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
