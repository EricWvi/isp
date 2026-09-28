package routing

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"isp/internal/config"
	"isp/internal/state"
)

func testConfig(groups ...config.Provider) config.Config {
	cfg := config.Defaults()
	cfg.Providers = groups
	return cfg
}

func group(id string, proxies ...config.Proxy) config.Provider {
	return config.Provider{ID: id, Type: "proxy-seller", Enabled: true, Proxies: proxies}
}

func proxy(id string) config.Proxy {
	return config.Proxy{ID: id, Host: "192.0.2.1", Port: 1080, Enabled: true}
}

func testStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedHealth(t *testing.T, s *state.Store, refs ...Ref) {
	t.Helper()
	for _, ref := range refs {
		if err := s.SaveHealth(context.Background(), state.Health{ProviderID: ref.ProviderID, ProxyID: ref.ProxyID, Status: "healthy"}); err != nil {
			t.Fatal(err)
		}
	}
}

func openRouter(t *testing.T, cfg config.Config, store SelectionStore) *Router {
	t.Helper()
	r, err := Open(context.Background(), cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func selected(r *Router) Ref {
	v := r.Selection()
	return Ref{v.ProviderID, v.ProxyID}
}

func TestStartupAndProviderRing(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	cfg := testConfig(group("first", proxy("a"), proxy("b")), group("second", proxy("c")), group("third", proxy("d")))
	seedHealth(t, s, Ref{"first", "a"}, Ref{"first", "b"}, Ref{"second", "c"})
	r := openRouter(t, cfg, s)
	if got := selected(r); got != (Ref{"first", "a"}) {
		t.Fatalf("startup selected %v", got)
	}
	if err := r.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := selected(r); got != (Ref{"second", "c"}) {
		t.Fatalf("ring selected %v", got)
	}
	if err := r.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := selected(r); got != (Ref{"first", "a"}) {
		t.Fatalf("ring wrap selected %v", got)
	}
	if err := r.HealthChanged(ctx, Ref{"second", "c"}, "unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := r.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := selected(r); got != (Ref{"first", "b"}) {
		t.Fatalf("provider internal rotation selected %v", got)
	}
}

func TestNoCandidateAndSingleProxy(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	r := openRouter(t, testConfig(group("only", proxy("one"))), s)
	if _, ok := r.Snapshot(); ok || selected(r) != (Ref{}) {
		t.Fatal("unknown proxy must not be auto-selected at startup")
	}
	if err := r.Rotate(ctx); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("rotate without candidate: %v", err)
	}
	if err := r.Select(ctx, Ref{"only", "one"}); err != nil {
		t.Fatal(err)
	}
	if snapshot, ok := r.Snapshot(); !ok || snapshot.Status != "unknown" {
		t.Fatalf("manual selection of unknown proxy: %+v, %v", snapshot, ok)
	}
	if err := r.HealthChanged(ctx, Ref{"only", "one"}, "healthy"); err != nil {
		t.Fatal(err)
	}
	if err := r.Rotate(ctx); !errors.Is(err, ErrNoAlternative) || selected(r) != (Ref{"only", "one"}) {
		t.Fatalf("single proxy rotation: %v, %v", err, selected(r))
	}
	if err := r.SetAutoSwitch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := r.HealthChanged(ctx, Ref{"only", "one"}, "unavailable"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("unavailable with no alternative: %v", err)
	}
	if _, ok := r.Snapshot(); ok {
		t.Fatal("confirmed unavailable proxy exposed to gateway")
	}
	if selected(r) != (Ref{"only", "one"}) {
		t.Fatal("current choice should be retained without alternative")
	}
}

func TestAutomaticFailoverAndStaleConcurrentEvents(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	cfg := testConfig(group("first", proxy("a")), group("second", proxy("b")), group("third", proxy("c")))
	seedHealth(t, s, Ref{"first", "a"}, Ref{"second", "b"}, Ref{"third", "c"})
	r := openRouter(t, cfg, s)
	if err := r.SetAutoSwitch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if selected(r) != (Ref{"first", "a"}) {
		t.Fatal("enabling automatic switch must not rotate")
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.HealthChanged(ctx, Ref{"first", "a"}, "unavailable"); err != nil {
				t.Errorf("health event: %v", err)
			}
		}()
	}
	wg.Wait()
	if selected(r) != (Ref{"second", "b"}) || r.Selection().SwitchReason != "automatic-failover" {
		t.Fatalf("concurrent failover: %+v", r.Selection())
	}
	before := r.Selection()
	if err := r.HealthChanged(ctx, Ref{"first", "a"}, "unavailable"); err != nil || r.Selection() != before {
		t.Fatalf("stale event changed choice: %+v, %v", r.Selection(), err)
	}
	stored, err := s.LoadSelection(ctx)
	if err != nil || stored != before {
		t.Fatalf("persisted selection: %+v, %v", stored, err)
	}
}

func TestHealthyAlternativeArrivingAfterFailureTriggersFailover(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	cfg := testConfig(group("first", proxy("a")), group("second", proxy("b")))
	seedHealth(t, s, Ref{"first", "a"})
	r := openRouter(t, cfg, s)
	if err := r.SetAutoSwitch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := r.HealthChanged(ctx, Ref{"first", "a"}, "unavailable"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("expected no candidate yet: %v", err)
	}
	if err := r.HealthChanged(ctx, Ref{"second", "b"}, "healthy"); err != nil || selected(r) != (Ref{"second", "b"}) {
		t.Fatalf("late healthy candidate did not trigger failover: %v, %v", selected(r), err)
	}
}

func TestRecoveryKeepsEnabledChoiceAndAutoSwitch(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	cfg := testConfig(group("first", proxy("a")), group("second", proxy("b")))
	seedHealth(t, s, Ref{"first", "a"}, Ref{"second", "b"})
	r := openRouter(t, cfg, s)
	if err := r.SetAutoSwitch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := r.Select(ctx, Ref{"second", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveHealth(ctx, state.Health{ProviderID: "second", ProxyID: "b", Status: "unavailable"}); err != nil {
		t.Fatal(err)
	}
	before := r.Selection()
	restarted := openRouter(t, cfg, s)
	if restarted.Selection() != before {
		t.Fatalf("choice not restored: %+v", restarted.Selection())
	}
	if _, ok := restarted.Snapshot(); ok {
		t.Fatal("restored unavailable choice should fail gateway reads")
	}
	if err := restarted.SetAutoSwitch(ctx, true); err != nil || restarted.Selection() != before {
		t.Fatal("re-enabling same setting rotated proxy")
	}
	if err := restarted.HealthChanged(ctx, Ref{"second", "b"}, "unavailable"); err != nil || selected(restarted) != (Ref{"first", "a"}) {
		t.Fatalf("fresh failed check should trigger switch: %v, %v", selected(restarted), err)
	}
}

func TestReconfigureAndSnapshotImmutability(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	cfg := testConfig(group("seller", proxy("one"), proxy("two")))
	seedHealth(t, s, Ref{"seller", "one"}, Ref{"seller", "two"})
	r := openRouter(t, cfg, s)
	before, ok := r.Snapshot()
	if !ok {
		t.Fatal("expected current proxy")
	}
	changed := testConfig(group("seller", proxy("one"), proxy("two")))
	changed.Providers[0].Proxies[0].Host = "192.0.2.22"
	if err := r.Reconfigure(ctx, changed); err != nil {
		t.Fatal(err)
	}
	after, ok := r.Snapshot()
	if !ok || after.Proxy.Host != "192.0.2.22" || after.Status != "unknown" || before.Proxy.Host != "192.0.2.1" || after.SelectedAt != before.SelectedAt {
		t.Fatalf("edit snapshot: before %+v, after %+v", before, after)
	}
	restarted := openRouter(t, changed, s)
	if snapshot, ok := restarted.Snapshot(); !ok || snapshot.Status != "unknown" {
		t.Fatalf("stale health restored after edit: %+v, %v", snapshot, ok)
	}
	changed.Providers[0].Proxies[0].Enabled = false
	if err := r.Reconfigure(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if selected(r) != (Ref{}) {
		t.Fatal("disabled current should clear choice when automatic switch is off")
	}
	if err := r.HealthChanged(ctx, Ref{"seller", "two"}, "healthy"); err != nil || selected(r) != (Ref{}) {
		t.Fatal("health event switched after current was disabled with automatic switch off")
	}
	if err := r.SetAutoSwitch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := r.Select(ctx, Ref{"seller", "one"}); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("disabled proxy selected: %v", err)
	}
	if err := r.HealthChanged(ctx, Ref{"seller", "two"}, "healthy"); err != nil || selected(r) != (Ref{"seller", "two"}) {
		t.Fatalf("empty choice did not recover: %v, %v", selected(r), err)
	}
	seedHealth(t, s, Ref{"seller", "one"})
	if err := r.HealthChanged(ctx, Ref{"seller", "one"}, "healthy"); err != nil {
		t.Fatal(err)
	}
	reenabled := proxy("one")
	reenabled.Host = "192.0.2.22"
	if err := r.Reconfigure(ctx, testConfig(group("seller", reenabled))); err != nil {
		t.Fatal(err)
	}
	if selected(r) != (Ref{"seller", "one"}) {
		t.Fatalf("deleted current did not select alternative: %v", selected(r))
	}
}

func TestRemovedPersistedChoiceAndOrphanHealth(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.SaveSelection(ctx, state.Selection{ProviderID: "removed", ProxyID: "old", AutoSwitch: true, SelectedAt: time.Now(), SwitchReason: "manual-select"}); err != nil {
		t.Fatal(err)
	}
	seedHealth(t, s, Ref{"removed", "old"}, Ref{"seller", "new"})
	r := openRouter(t, testConfig(group("seller", proxy("new"))), s)
	if selected(r) != (Ref{"seller", "new"}) || !r.Selection().AutoSwitch {
		t.Fatalf("invalid persisted choice not replaced: %+v", r.Selection())
	}
}

func TestInvalidPersistedProxyStartsAtNextProvider(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.SaveSelection(ctx, state.Selection{ProviderID: "first", ProxyID: "deleted", AutoSwitch: true}); err != nil {
		t.Fatal(err)
	}
	seedHealth(t, s, Ref{"first", "a"}, Ref{"second", "b"})
	r := openRouter(t, testConfig(group("first", proxy("a")), group("second", proxy("b"))), s)
	if selected(r) != (Ref{"second", "b"}) {
		t.Fatalf("restored rotation started at wrong provider: %v", selected(r))
	}
}

func TestRemovedChoiceStaysEmptyWithAutomaticSwitchOff(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.SaveSelection(ctx, state.Selection{ProviderID: "removed", ProxyID: "old", AutoSwitch: false}); err != nil {
		t.Fatal(err)
	}
	seedHealth(t, s, Ref{"seller", "new"})
	cfg := testConfig(group("seller", proxy("new")))
	r := openRouter(t, cfg, s)
	if selected(r) != (Ref{}) || r.Selection().SwitchReason != "startup-invalid" {
		t.Fatalf("startup switched while disabled: %+v", r.Selection())
	}
	if err := r.HealthChanged(ctx, Ref{"seller", "new"}, "healthy"); err != nil || selected(r) != (Ref{}) {
		t.Fatalf("health event switched while disabled: %v, %v", selected(r), err)
	}
	restarted := openRouter(t, cfg, s)
	if selected(restarted) != (Ref{}) {
		t.Fatal("restart switched while disabled")
	}
}

type failingStore struct{ selection state.Selection }

func (s *failingStore) LoadSelection(context.Context) (state.Selection, error) {
	return s.selection, nil
}
func (s *failingStore) SaveSelection(context.Context, state.Selection) error {
	return errors.New("disk unavailable")
}
func (s *failingStore) LoadHealth(context.Context) ([]state.Health, error) { return nil, nil }
func (s *failingStore) ForgetHealth(context.Context, []state.ProxyKey) error {
	return nil
}

func TestPersistenceFailureDoesNotPublishChoice(t *testing.T) {
	ctx := context.Background()
	r := openRouter(t, testConfig(group("seller", proxy("one"))), &failingStore{})
	if err := r.Select(ctx, Ref{"seller", "one"}); err == nil {
		t.Fatal("expected persistence error")
	}
	if selected(r) != (Ref{}) {
		t.Fatal("failed selection reached memory")
	}
	if err := r.SetAutoSwitch(ctx, true); err == nil || r.Selection().AutoSwitch {
		t.Fatal("failed setting change reached memory")
	}
}

func TestSelectionRevisionRejectsConcurrentChange(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	cfg := testConfig(group("seller", proxy("one"), proxy("two")))
	r := openRouter(t, cfg, s)
	initial := r.SelectionRevision()
	if err := r.SelectIfRevision(ctx, initial, Ref{"seller", "one"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SelectIfRevision(ctx, initial, Ref{"seller", "two"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale selection accepted: %v", err)
	}
	if err := r.SetAutoSwitchIfRevision(ctx, initial, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale toggle accepted: %v", err)
	}
	if selected(r) != (Ref{"seller", "one"}) || r.Selection().AutoSwitch {
		t.Fatalf("conflicting request changed state: %+v", r.Selection())
	}
}
