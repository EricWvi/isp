package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"

	"isp/internal/config"
	"isp/internal/health"
	"isp/internal/routing"
	"isp/internal/state"
)

func newTestApp(t *testing.T) (*App, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.Defaults()
	cfg.Server.Database = filepath.Join(filepath.Dir(path), "state.db")
	cfg.Providers = []config.Provider{{ID: "seller", Type: "proxy-seller", Enabled: true, Proxies: []config.Proxy{
		{ID: "one", Host: "192.0.2.1", Port: 1080, Enabled: true, Username: "alice", Password: "secret"},
	}}}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(ctx, cfg.Server.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.SaveHealth(ctx, state.Health{ProviderID: "seller", ProxyID: "one", Status: "healthy"}); err != nil {
		t.Fatal(err)
	}
	router, err := routing.Open(ctx, cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := health.New(ctx, cfg, store, router, func(context.Context, config.Proxy, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return &App{Config: file, Router: router, Health: manager, Store: store}, path
}

func send(t *testing.T, handler http.Handler, method, path, body, revision string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if revision != "" {
		request.Header.Set("If-Match", `"`+revision+`"`)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func readState(t *testing.T, response *httptest.ResponseRecorder) stateView {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	var state stateView
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestProxyCRUDConflictAndHealthReset(t *testing.T) {
	app, path := newTestApp(t)
	handler := app.Handler()
	initialResponse := send(t, handler, "GET", "/api/state", "", "")
	initial := readState(t, initialResponse)
	if strings.Contains(initialResponse.Body.String(), "secret") || !initial.Providers[0].Proxies[0].HasPassword {
		t.Fatal("management state exposed password or lost its presence flag")
	}
	created := readState(t, send(t, handler, "POST", "/api/providers/seller/proxies", `{"id":"two","host":"192.0.2.2","port":1080,"enabled":true}`, initial.ConfigRevision))
	if len(created.Providers[0].Proxies) != 2 || created.ConfigRevision == initial.ConfigRevision {
		t.Fatalf("create failed: %+v", created)
	}
	stale := send(t, handler, "POST", "/api/providers/seller/proxies", `{"id":"three","host":"192.0.2.3","port":1080,"enabled":true}`, initial.ConfigRevision)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale create status %d: %s", stale.Code, stale.Body.String())
	}
	edited := readState(t, send(t, handler, "PUT", "/api/providers/seller/proxies/one", `{"name":"edited","host":"192.0.2.11","port":1080,"enabled":true,"username":"alice"}`, created.ConfigRevision))
	if edited.Providers[0].Proxies[0].Status != "unknown" || !edited.Providers[0].Proxies[0].HasPassword {
		t.Fatalf("edit did not reset health or retain secret: %+v", edited.Providers[0].Proxies[0])
	}
	records, err := app.Store.LoadHealth(context.Background())
	if err != nil || len(records) != 0 {
		t.Fatalf("stale health remained in SQLite: %+v, %v", records, err)
	}
	disabled := readState(t, send(t, handler, "PATCH", "/api/providers/seller/proxies/one/enabled", `{"enabled":false}`, edited.ConfigRevision))
	if disabled.Providers[0].Proxies[0].Enabled || disabled.Selection.ProxyID != "" {
		t.Fatalf("disabling current did not clear selection: %+v", disabled)
	}
	deleted := readState(t, send(t, handler, "DELETE", "/api/providers/seller/proxies/one", "", disabled.ConfigRevision))
	if len(deleted.Providers[0].Proxies) != 1 || deleted.Providers[0].Proxies[0].ID != "two" {
		t.Fatalf("delete failed: %+v", deleted)
	}
	onDisk, err := config.Load(path)
	if err != nil || len(onDisk.Providers[0].Proxies) != 1 || onDisk.Providers[0].Proxies[0].ID != "two" {
		t.Fatalf("YAML did not match runtime: %+v, %v", onDisk, err)
	}
}

func TestSelectionActionsAndPreconditions(t *testing.T) {
	app, _ := newTestApp(t)
	handler := app.Handler()
	initial := readState(t, send(t, handler, "GET", "/api/state", "", ""))
	if initial.Selection.ProxyID != "one" {
		t.Fatal("healthy proxy not selected at startup")
	}
	missing := send(t, handler, "POST", "/api/rotate", "", "")
	if missing.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing revision status %d", missing.Code)
	}
	enabled := readState(t, send(t, handler, "PUT", "/api/auto-switch", `{"enabled":true}`, initial.SelectionRevision))
	if !enabled.Selection.AutoSwitch || enabled.SelectionRevision == initial.SelectionRevision {
		t.Fatalf("auto switch did not change: %+v", enabled.Selection)
	}
	stale := send(t, handler, "PUT", "/api/selection", `{"provider_id":"seller","proxy_id":"one"}`, initial.SelectionRevision)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale selection status %d", stale.Code)
	}
	rotated := send(t, handler, "POST", "/api/rotate", "", enabled.SelectionRevision)
	if rotated.Code != http.StatusConflict || !strings.Contains(rotated.Body.String(), "no other available proxy") {
		t.Fatalf("single-proxy rotation feedback: %d, %s", rotated.Code, rotated.Body.String())
	}
	unknown := send(t, handler, "PUT", "/api/selection", `{"provider_id":"seller","proxy_id":"missing"}`, enabled.SelectionRevision)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown proxy selection status %d", unknown.Code)
	}
}

func TestHTTPPoolSelectionIsIndependent(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := context.Background()
	if err := app.Config.Update(func(cfg *config.Config) error {
		cfg.Providers = append(cfg.Providers, config.Provider{ID: "http-seller", Type: "proxy-seller", Protocol: "http", Enabled: true, Proxies: []config.Proxy{
			{ID: "http-one", Host: "192.0.2.10", Port: 8080, Enabled: true},
			{ID: "http-two", Host: "192.0.2.11", Port: 8080, Enabled: true},
		}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"http-one", "http-two"} {
		if err := app.Store.SaveHealth(ctx, state.Health{ProviderID: "http-seller", ProxyID: id, Status: "healthy"}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := app.Config.Snapshot()
	httpRouter, err := routing.Open(ctx, cfg.ForProtocol("http"), app.Store.HTTPSelectionStore())
	if err != nil {
		t.Fatal(err)
	}
	httpManager, err := health.New(ctx, cfg.ForProtocol("http"), app.Store, httpRouter, func(context.Context, config.Proxy, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	app.HTTPRouter, app.HTTPHealth = httpRouter, httpManager
	initial := readState(t, send(t, app.Handler(), "GET", "/api/state", "", ""))
	if initial.Selection.ProxyID != "one" || initial.HTTPSelection.ProxyID != "http-one" || initial.Providers[1].Protocol != "http" {
		t.Fatalf("unexpected pool state: %+v", initial)
	}
	selected := readState(t, send(t, app.Handler(), "PUT", "/api/http/selection", `{"provider_id":"http-seller","proxy_id":"http-two"}`, initial.HTTPSelectionRevision))
	if selected.HTTPSelection.ProxyID != "http-two" || selected.Selection.ProxyID != "one" || selected.SelectionRevision != initial.SelectionRevision {
		t.Fatalf("HTTP selection changed SOCKS pool: %+v", selected)
	}
	enabled := readState(t, send(t, app.Handler(), "PUT", "/api/http/auto-switch", `{"enabled":true}`, selected.HTTPSelectionRevision))
	if !enabled.HTTPSelection.AutoSwitch || enabled.Selection.AutoSwitch {
		t.Fatalf("HTTP auto-switch changed SOCKS pool: %+v", enabled)
	}
	wrongPool := send(t, app.Handler(), "PUT", "/api/selection", `{"provider_id":"http-seller","proxy_id":"http-one"}`, enabled.SelectionRevision)
	if wrongPool.Code != http.StatusNotFound {
		t.Fatalf("cross-pool selection status %d", wrongPool.Code)
	}
	created := readState(t, send(t, app.Handler(), "POST", "/api/providers/http-seller/proxies", `{"id":"http-three","host":"192.0.2.12","port":8080,"enabled":true}`, enabled.ConfigRevision))
	if len(created.Providers[1].Proxies) != 3 || created.Selection.ProxyID != "one" || created.HTTPSelection.ProxyID != "http-two" {
		t.Fatalf("HTTP config update changed selections: %+v", created)
	}
}

func TestConcurrentConfigWritesRejectStaleRevision(t *testing.T) {
	app, _ := newTestApp(t)
	handler := app.Handler()
	initial := readState(t, send(t, handler, "GET", "/api/state", "", ""))
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, id := range []string{"two", "three"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := send(t, handler, "POST", "/api/providers/seller/proxies", `{"id":"`+id+`","host":"192.0.2.2","port":1080,"enabled":true}`, initial.ConfigRevision)
			results <- response.Code
		}()
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent results: %+v", counts)
	}
}

func TestWriteFailureKeepsRuntimeConfig(t *testing.T) {
	app, path := newTestApp(t)
	before := readState(t, send(t, app.Handler(), "GET", "/api/state", "", ""))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	response := send(t, app.Handler(), "POST", "/api/providers/seller/proxies", `{"id":"two","host":"192.0.2.2","port":1080,"enabled":true}`, before.ConfigRevision)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "cannot write configuration") {
		t.Fatalf("write failure response: %d, %s", response.Code, response.Body.String())
	}
	after := readState(t, send(t, app.Handler(), "GET", "/api/state", "", ""))
	if after.ConfigRevision != before.ConfigRevision || len(after.Providers[0].Proxies) != 1 {
		t.Fatal("failed write changed runtime configuration")
	}
}

func TestStateReportsBuildVersion(t *testing.T) {
	app, _ := newTestApp(t)
	app.Version = "v9.8.7"
	state := readState(t, send(t, app.Handler(), http.MethodGet, "/api/state", "", ""))
	if state.Version != "v9.8.7" {
		t.Fatalf("version %q", state.Version)
	}
}
