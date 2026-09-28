package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const sample = `server:
  socks5_listen: 127.0.0.1:30001
providers:
  - id: proxy-seller
    type: proxy-seller
    enabled: true
    proxies:
      - id: first
        host: 192.0.2.10
        port: 1080
        enabled: true
`

func TestDecodeDefaultsAndValidation(t *testing.T) {
	cfg, err := Decode([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HealthCheck.FailureThreshold != 3 || cfg.Server.HTTPListen != "127.0.0.1:8080" ||
		!cfg.Server.SOCKS5Enabled || cfg.Server.HTTPProxyEnabled || cfg.Server.HTTPProxyListen != "127.0.0.1:30002" {
		t.Fatalf("defaults missing: %+v", cfg)
	}
	for _, tc := range []struct{ name, input string }{
		{"low SOCKS port", strings.Replace(sample, "30001", "29999", 1)},
		{"duplicate proxy", sample + `  - id: other
    type: proxy-seller
    proxies:
      - id: first
        host: example.com
        port: 1080
`},
		{"invalid host", strings.Replace(sample, "192.0.2.10", "bad host", 1)},
		{"invalid duration", sample + "health_check:\n  timeout: forever\n"},
		{"unknown field", sample + "unexpected: true\n"},
		{"multiple documents", sample + "---\nserver: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode([]byte(tc.input)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestProxyListenerCombinations(t *testing.T) {
	for _, tc := range []struct {
		name, flags string
		socks, http bool
	}{
		{"both-off", "  socks5_enabled: false\n  http_proxy_enabled: false\n", false, false},
		{"socks-only", "  socks5_enabled: true\n  http_proxy_enabled: false\n", true, false},
		{"http-only", "  socks5_enabled: false\n  http_proxy_enabled: true\n", false, true},
		{"both-on", "  socks5_enabled: true\n  http_proxy_enabled: true\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(sample, "  socks5_listen: 127.0.0.1:30001\n", "  socks5_listen: 127.0.0.1:30001\n"+tc.flags, 1)
			cfg, err := Decode([]byte(input))
			if err != nil || cfg.Server.SOCKS5Enabled != tc.socks || cfg.Server.HTTPProxyEnabled != tc.http {
				t.Fatalf("listeners: %+v, %v", cfg.Server, err)
			}
		})
	}
	for _, input := range []string{
		"  http_proxy_enabled: true\n  http_proxy_listen: 127.0.0.1:29999\n",
		"  http_proxy_enabled: true\n  http_proxy_listen: 127.0.0.1:30001\n",
	} {
		if _, err := Decode([]byte(strings.Replace(sample, "  socks5_listen: 127.0.0.1:30001\n", "  socks5_listen: 127.0.0.1:30001\n"+input, 1))); err == nil {
			t.Fatal("expected invalid HTTP proxy listener to be rejected")
		}
	}
}

func TestProviderProtocolPoolsAndLegacyDefault(t *testing.T) {
	cfg, err := Decode([]byte(sample + `  - id: http-seller
    type: proxy-seller
    protocol: http
    enabled: true
    proxies:
      - id: http-first
        host: 192.0.2.11
        port: 8080
        enabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if groups := cfg.ForProtocol("socks5").Providers; len(groups) != 1 || groups[0].ID != "proxy-seller" {
		t.Fatalf("legacy SOCKS5 pool: %+v", groups)
	}
	if groups := cfg.ForProtocol("http").Providers; len(groups) != 1 || groups[0].ID != "http-seller" {
		t.Fatalf("HTTP pool: %+v", groups)
	}
	if _, err := Decode([]byte(strings.Replace(sample, "    type: proxy-seller", "    type: proxy-seller\n    protocol: ftp", 1))); err == nil {
		t.Fatal("unsupported upstream protocol accepted")
	}
}

func TestCorruptConfigIsRejectedWithoutOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	bad := []byte("server: [unterminated\n")
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt configuration was accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(bad) {
		t.Fatalf("corrupt configuration was overwritten: %q, %v", got, err)
	}
}

func TestFileUpdateSerializesAndKeepsFailedChangeOutOfMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(sample), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.Update(func(c *Config) error {
				c.Providers[0].Proxies = append(c.Providers[0].Proxies, Proxy{ID: "proxy-" + string(rune('a'+len(c.Providers[0].Proxies))), Host: "example.com", Port: 1080})
				return nil
			}); err != nil {
				t.Errorf("update: %v", err)
			}
		}()
	}
	wg.Wait()
	before := f.Snapshot()
	if len(before.Providers[0].Proxies) != 13 {
		t.Fatalf("lost updates: %d proxies", len(before.Providers[0].Proxies))
	}
	if err := f.Update(func(c *Config) error {
		c.Providers[0].Proxies[0].Port = 0
		return nil
	}); err == nil {
		t.Fatal("expected invalid update to fail")
	}
	if f.Snapshot().Providers[0].Proxies[0].Port != 1080 {
		t.Fatal("failed update reached memory")
	}
	onDisk, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk.Providers[0].Proxies) != 13 || onDisk.Providers[0].Proxies[0].Port != 1080 {
		t.Fatal("disk and memory diverged")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "providers:\n  - id: proxy-seller") {
		t.Fatal("expected stable two-space indentation")
	}
}

func TestFileUpdateWriteFailureDoesNotPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(sample), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(func(c *Config) error {
		c.Providers[0].Proxies[0].Port = 2080
		return nil
	}); err == nil {
		t.Fatal("expected rename to fail")
	}
	if f.Snapshot().Providers[0].Proxies[0].Port != 1080 {
		t.Fatal("failed write reached memory")
	}
}

func TestFileRevisionRejectsStaleUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(sample), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, initial := f.SnapshotWithRevision()
	updated, err := f.UpdateIfRevision(initial, func(c *Config) error {
		c.Providers[0].Proxies[0].Name = "updated"
		return nil
	})
	if err != nil || updated == initial {
		t.Fatalf("revision did not advance: %q, %v", updated, err)
	}
	if _, err := f.UpdateIfRevision(initial, func(c *Config) error {
		c.Providers[0].Proxies[0].Name = "stale"
		return nil
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update accepted: %v", err)
	}
	if f.Snapshot().Providers[0].Proxies[0].Name != "updated" {
		t.Fatal("stale update changed configuration")
	}
}
