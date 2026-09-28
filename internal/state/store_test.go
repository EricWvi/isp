package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	wantHTTP := Selection{ProviderID: "http-seller", ProxyID: "http-first", AutoSwitch: false, SelectedAt: when, SwitchReason: "manual-select"}
	if err := s.HTTPSelectionStore().SaveSelection(ctx, wantHTTP); err != nil {
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
	gotHTTP, err := s.HTTPSelectionStore().LoadSelection(ctx)
	if err != nil || gotHTTP != wantHTTP {
		t.Fatalf("HTTP selection: got %+v, %v", gotHTTP, err)
	}
	gotHealth, err := s.LoadHealth(ctx)
	if err != nil || len(gotHealth) != 1 || gotHealth[0] != wantHealth {
		t.Fatalf("health: got %+v, %v", gotHealth, err)
	}
}

func TestVersionOneDatabaseMigratesWithoutChangingSOCKSSelection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	want := Selection{ProviderID: "seller", ProxyID: "first", AutoSwitch: true}
	if err := s.SaveSelection(ctx, want); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE selection_http"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
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
	got, err := s.LoadSelection(ctx)
	if err != nil || got != want {
		t.Fatalf("SOCKS selection changed: %+v, %v", got, err)
	}
	httpSelection, err := s.HTTPSelectionStore().LoadSelection(ctx)
	if err != nil || httpSelection != (Selection{}) {
		t.Fatalf("HTTP selection was not initialized: %+v, %v", httpSelection, err)
	}
}

func TestCorruptDatabaseFailsWithoutReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "isp.db")
	bad := []byte("not a SQLite database")
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(context.Background(), path); err == nil {
		store.Close()
		t.Fatal("corrupt database was accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(bad) {
		t.Fatalf("corrupt file was overwritten: %q, %v", got, err)
	}
}

func TestDatabaseFullDoesNotPublishPartialHealth(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "isp.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var pages int
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA max_page_count = "+fmt.Sprint(pages)); err != nil {
		t.Fatal(err)
	}
	err = s.SaveHealth(ctx, Health{ProviderID: "seller", ProxyID: "first", Status: "suspect", LastError: strings.Repeat("x", 1<<20)})
	if err == nil {
		t.Fatal("expected database-full error")
	}
	health, loadErr := s.LoadHealth(ctx)
	if loadErr != nil || len(health) != 0 {
		t.Fatalf("partial health write: %+v, %v", health, loadErr)
	}
}
