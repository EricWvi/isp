package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStateSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "isp.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 28, 1, 2, 3, 4, time.UTC)
	wantSelection := Selection{ProviderID: "proxy-seller", ProxyID: "first", AutoSwitch: true, SelectedAt: when, SwitchReason: "manual"}
	if err := s.SaveSelection(ctx, wantSelection); err != nil {
		t.Fatal(err)
	}
	wantHealth := Health{ProviderID: "proxy-seller", ProxyID: "first", Status: "suspect", ConsecutiveFailures: 2, LastCheckedAt: when, LastResult: "failure", LastError: "timeout", NextCheckAt: when.Add(time.Minute), BackoffStage: 2}
	if err := s.SaveHealth(ctx, wantHealth); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gotSelection, err := s.LoadSelection(ctx)
	if err != nil || gotSelection != wantSelection {
		t.Fatalf("selection: got %+v, %v", gotSelection, err)
	}
	gotHealth, err := s.LoadHealth(ctx)
	if err != nil || len(gotHealth) != 1 || gotHealth[0] != wantHealth {
		t.Fatalf("health: got %+v, %v", gotHealth, err)
	}
}
