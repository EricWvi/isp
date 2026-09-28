package health

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"isp/internal/config"
	"isp/internal/routing"
	"isp/internal/state"
)

func healthConfig(proxies ...config.Proxy) config.Config {
	cfg := config.Defaults()
	cfg.Providers = []config.Provider{{ID: "seller", Type: "proxy-seller", Enabled: true, Proxies: proxies}}
	return cfg
}

func healthProxy(id string) config.Proxy {
	return config.Proxy{ID: id, Host: "192.0.2.1", Port: 1080, Enabled: true}
}

func TestFailureThresholdBackoffAndRecovery(t *testing.T) {
	settings := config.Defaults().HealthCheck
	now := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	h := state.Health{ProviderID: "seller", ProxyID: "one", Status: "unknown"}
	for i, want := range []struct {
		status string
		delay  time.Duration
	}{{"suspect", 5 * time.Minute}, {"suspect", 10 * time.Minute}, {"unavailable", 20 * time.Minute}} {
		h = afterProbe(h, errors.New("dial failed"), now, settings, 0.5)
		if h.Status != want.status || h.ConsecutiveFailures != i+1 || h.BackoffStage != i+1 || h.NextCheckAt.Sub(now) != want.delay {
			t.Fatalf("failure %d: %+v", i+1, h)
		}
	}
	h = afterProbe(h, nil, now, settings, 0.5)
	if h.Status != "healthy" || h.ConsecutiveFailures != 0 || h.BackoffStage != 0 || h.LastError != "" || h.NextCheckAt.Sub(now) != 6*time.Minute+30*time.Second {
		t.Fatalf("recovery: %+v", h)
	}
	if next := afterProbe(h, errors.New("again"), now, settings, 0); next.NextCheckAt.Sub(now) != 4*time.Minute {
		t.Fatalf("jitter not applied: %+v", next)
	}
	for i := 0; i < 20; i++ {
		h = afterProbe(h, errors.New("still down"), now, settings, 0.99)
	}
	if h.NextCheckAt.Sub(now) > time.Hour || h.BackoffStage != 20 {
		t.Fatalf("backoff exceeded cap: %+v", h)
	}
}

func TestIndependentChecksPersistAndTriggerAutomaticFailover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := healthConfig(healthProxy("first"), healthProxy("second"))
	cfg.HealthCheck.FailureThreshold = 1
	cfg.HealthCheck.IntervalMin = config.Duration(time.Hour)
	cfg.HealthCheck.IntervalMax = config.Duration(time.Hour)
	cfg.HealthCheck.BackoffMax = config.Duration(2 * time.Hour)
	router, err := routing.Open(ctx, cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Select(ctx, routing.Ref{ProviderID: "seller", ProxyID: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := router.SetAutoSwitch(ctx, true); err != nil {
		t.Fatal(err)
	}
	manager, err := New(ctx, cfg, store, router, func(_ context.Context, proxy config.Proxy, _ string) error {
		if proxy.ID == "first" {
			return errors.New("upstream unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	deadline := time.After(2 * time.Second)
	for {
		records, err := store.LoadHealth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		choice := router.Selection()
		if len(records) == 2 && choice.ProxyID == "second" {
			if records[0].Status != "unavailable" || records[1].Status != "healthy" || records[0].NextCheckAt.IsZero() || records[1].NextCheckAt.IsZero() {
				t.Fatalf("health records: %+v", records)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("checks did not finish: records=%+v choice=%+v", records, choice)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	choice, err := reopened.LoadSelection(context.Background())
	if err != nil || choice.ProxyID != "second" || choice.SwitchReason != "automatic-failover" {
		t.Fatalf("choice after restart: %+v, %v", choice, err)
	}
	records, err := reopened.LoadHealth(context.Background())
	if err != nil || len(records) != 2 || records[0].BackoffStage != 1 {
		t.Fatalf("health after restart: %+v, %v", records, err)
	}
}

func TestRecoveredScheduleAndConnectionErrorRecheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := healthConfig(healthProxy("one"))
	cfg.HealthCheck.IntervalMin = config.Duration(time.Hour)
	cfg.HealthCheck.IntervalMax = config.Duration(time.Hour)
	cfg.HealthCheck.BackoffMax = config.Duration(2 * time.Hour)
	ref := routing.Ref{ProviderID: "seller", ProxyID: "one"}
	original := state.Health{ProviderID: ref.ProviderID, ProxyID: ref.ProxyID, Status: "healthy", ConsecutiveSuccesses: 2, LastCheckedAt: time.Now().Add(-time.Minute).UTC(), NextCheckAt: time.Now().Add(time.Hour).UTC()}
	if err := store.SaveHealth(ctx, original); err != nil {
		t.Fatal(err)
	}
	router, err := routing.Open(ctx, cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan struct{}, 1)
	manager, err := New(ctx, cfg, store, router, func(context.Context, config.Proxy, string) error {
		calls <- struct{}{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	select {
	case <-calls:
		t.Fatal("restarted manager ignored persisted future check time")
	case <-time.After(100 * time.Millisecond):
	}
	if err := manager.ReportConnectionError(ctx, ref); err != nil {
		t.Fatal(err)
	}
	records, err := store.LoadHealth(ctx)
	if err != nil || len(records) != 1 || records[0].ConsecutiveSuccesses != 2 || records[0].ConsecutiveFailures != 0 || records[0].NextCheckAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("early recheck state: %+v, %v", records, err)
	}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("connection error did not trigger early check")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnavailableCurrentStaysSelectedWithAutomaticSwitchOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := healthConfig(healthProxy("one"))
	cfg.HealthCheck.FailureThreshold = 1
	cfg.HealthCheck.IntervalMin = config.Duration(time.Hour)
	cfg.HealthCheck.IntervalMax = config.Duration(time.Hour)
	cfg.HealthCheck.BackoffMax = config.Duration(2 * time.Hour)
	router, err := routing.Open(ctx, cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Select(ctx, routing.Ref{ProviderID: "seller", ProxyID: "one"}); err != nil {
		t.Fatal(err)
	}
	manager, err := New(ctx, cfg, store, router, func(context.Context, config.Proxy, string) error {
		return errors.New("unreachable")
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	deadline := time.After(2 * time.Second)
	for {
		records, err := store.LoadHealth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 1 && records[0].Status == "unavailable" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("probe did not confirm failure")
		case <-time.After(10 * time.Millisecond):
		}
	}
	choice := router.Selection()
	if choice.ProxyID != "one" || choice.AutoSwitch {
		t.Fatalf("current proxy changed with auto switch off: %+v", choice)
	}
	if _, ok := router.Snapshot(); ok {
		t.Fatal("confirmed unavailable proxy exposed to new connections")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
