package routing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"isp/internal/config"
	"isp/internal/provider"
	"isp/internal/state"
)

var (
	ErrNoAvailable   = errors.New("no available proxy")
	ErrNoAlternative = errors.New("no other available proxy; current selection kept")
	ErrNotEnabled    = errors.New("proxy does not exist or is disabled")
)

type Ref struct {
	ProviderID string
	ProxyID    string
}

type Snapshot struct {
	Ref          Ref
	ProviderName string
	Proxy        config.Proxy
	Status       string
	SelectedAt   time.Time
	Reason       string
}

type SelectionStore interface {
	LoadSelection(context.Context) (state.Selection, error)
	SaveSelection(context.Context, state.Selection) error
	LoadHealth(context.Context) ([]state.Health, error)
	ForgetHealth(context.Context, []state.ProxyKey) error
}

type Router struct {
	mu        sync.RWMutex
	store     SelectionStore
	providers []provider.Provider
	configs   []config.Provider
	health    map[Ref]string
	selection state.Selection
}

// Open loads the persisted choice, then reconciles it with YAML. A still
// enabled choice survives even if its health is unknown or unavailable.
func Open(ctx context.Context, cfg config.Config, store SelectionStore) (*Router, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	selection, err := store.LoadSelection(ctx)
	if err != nil {
		return nil, err
	}
	records, err := store.LoadHealth(ctx)
	if err != nil {
		return nil, err
	}
	r := &Router{store: store, health: make(map[Ref]string), selection: selection}
	for _, record := range records {
		r.health[Ref{record.ProviderID, record.ProxyID}] = record.Status
	}
	r.setProviders(cfg.Providers)
	current := Ref{selection.ProviderID, selection.ProxyID}
	if current.ProviderID != "" && r.findEnabled(current) {
		return r, nil
	}
	if current != (Ref{}) && !selection.AutoSwitch {
		if err := r.saveChoice(ctx, Ref{}, "startup-invalid"); err != nil {
			return nil, err
		}
		return r, nil
	}
	if current == (Ref{}) && selection.SwitchReason != "" && !selection.AutoSwitch {
		return r, nil
	}
	next, ok := r.next(current, false)
	if ok {
		if err := r.saveChoice(ctx, next, "startup"); err != nil {
			return nil, err
		}
	} else if current != (Ref{}) {
		if err := r.saveChoice(ctx, Ref{}, "startup-no-candidate"); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Snapshot is the gateway-facing read. It returns no proxy when the current
// proxy is confirmed unavailable. The returned value is independent of later
// selection and configuration changes.
func (r *Router) Snapshot() (Snapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref := Ref{r.selection.ProviderID, r.selection.ProxyID}
	if ref == (Ref{}) || r.status(ref) == "unavailable" {
		return Snapshot{}, false
	}
	for i, cfg := range r.configs {
		if cfg.ID != ref.ProviderID || !cfg.Enabled {
			continue
		}
		for _, proxy := range cfg.Proxies {
			if proxy.ID == ref.ProxyID && proxy.Enabled {
				return Snapshot{Ref: ref, ProviderName: r.providers[i].DisplayName(), Proxy: proxy, Status: r.status(ref), SelectedAt: r.selection.SelectedAt, Reason: r.selection.SwitchReason}, true
			}
		}
	}
	return Snapshot{}, false
}

func (r *Router) Selection() state.Selection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.selection
}

func (r *Router) SetAutoSwitch(ctx context.Context, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.selection.AutoSwitch == enabled {
		return nil
	}
	next := r.selection
	next.AutoSwitch = enabled
	if err := r.store.SaveSelection(ctx, next); err != nil {
		return err
	}
	r.selection = next
	return nil
}

// Select accepts any enabled proxy, including one whose health is not yet
// confirmed. Manual selection does not change the automatic switch setting.
func (r *Router) Select(ctx context.Context, ref Ref) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.findEnabled(ref) {
		return ErrNotEnabled
	}
	if ref == (Ref{r.selection.ProviderID, r.selection.ProxyID}) {
		return nil
	}
	return r.saveChoice(ctx, ref, "manual-select")
}

func (r *Router) Rotate(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := Ref{r.selection.ProviderID, r.selection.ProxyID}
	next, ok := r.next(current, true)
	if !ok {
		if current != (Ref{}) {
			return ErrNoAlternative
		}
		return ErrNoAvailable
	}
	return r.saveChoice(ctx, next, "manual-rotate")
}

// HealthChanged is called after the health module persists its result. Stale
// failure reports for a former current proxy cannot trigger another switch.
func (r *Router) HealthChanged(ctx context.Context, ref Ref, status string) error {
	if !validStatus(status) {
		return fmt.Errorf("invalid health status %q", status)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health[ref] = status
	current := Ref{r.selection.ProviderID, r.selection.ProxyID}
	if current == (Ref{}) && status == "healthy" && (r.selection.AutoSwitch || r.selection.SwitchReason == "") {
		if next, ok := r.next(Ref{}, false); ok {
			return r.saveChoice(ctx, next, "health-available")
		}
	}
	if ref != current || status != "unavailable" || !r.selection.AutoSwitch {
		return nil
	}
	next, ok := r.next(current, true)
	if !ok {
		return ErrNoAvailable
	}
	return r.saveChoice(ctx, next, "automatic-failover")
}

// Reconfigure applies a validated YAML snapshot after its file write succeeds.
// If the selected proxy was removed or disabled, the automatic switch policy
// decides whether a replacement is selected.
func (r *Router) Reconfigure(ctx context.Context, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	oldProviders, oldConfigs := r.providers, r.configs
	changed := connectionChanges(oldConfigs, cfg.Providers)
	if err := r.store.ForgetHealth(ctx, changed); err != nil {
		return err
	}
	for _, key := range changed {
		delete(r.health, Ref{key.ProviderID, key.ProxyID})
	}
	r.setProviders(cfg.Providers)
	current := Ref{r.selection.ProviderID, r.selection.ProxyID}
	if current == (Ref{}) || r.findEnabled(current) {
		return nil
	}
	next := Ref{}
	if r.selection.AutoSwitch {
		if candidate, ok := r.next(current, false); ok {
			next = candidate
		}
	}
	if err := r.saveChoice(ctx, next, "configuration-changed"); err != nil {
		r.providers, r.configs = oldProviders, oldConfigs
		return err
	}
	return nil
}

func connectionChanges(old, next []config.Provider) []state.ProxyKey {
	prior := make(map[Ref]config.Proxy)
	for _, group := range old {
		for _, proxy := range group.Proxies {
			prior[Ref{group.ID, proxy.ID}] = proxy
		}
	}
	var changed []state.ProxyKey
	for _, group := range next {
		for _, proxy := range group.Proxies {
			previous, found := prior[Ref{group.ID, proxy.ID}]
			if !found || previous.Host != proxy.Host || previous.Port != proxy.Port || previous.Username != proxy.Username || previous.Password != proxy.Password {
				changed = append(changed, state.ProxyKey{ProviderID: group.ID, ProxyID: proxy.ID})
			}
		}
	}
	return changed
}

func (r *Router) setProviders(configs []config.Provider) {
	r.configs = make([]config.Provider, len(configs))
	r.providers = make([]provider.Provider, len(configs))
	for i, cfg := range configs {
		r.configs[i] = cfg
		r.configs[i].Proxies = append([]config.Proxy(nil), cfg.Proxies...)
		r.providers[i] = provider.NewProxySeller(cfg)
	}
}

func (r *Router) findEnabled(ref Ref) bool {
	for _, cfg := range r.configs {
		if cfg.ID != ref.ProviderID || !cfg.Enabled {
			continue
		}
		for _, proxy := range cfg.Proxies {
			if proxy.ID == ref.ProxyID && proxy.Enabled {
				return true
			}
		}
	}
	return false
}

func (r *Router) next(current Ref, excludeCurrent bool) (Ref, bool) {
	count := len(r.providers)
	if count == 0 {
		return Ref{}, false
	}
	currentIndex := -1
	for i, p := range r.providers {
		if p.ID() == current.ProviderID {
			currentIndex = i
			break
		}
	}
	for step := 1; step <= count; step++ {
		i := (currentIndex + step) % count
		if !r.configs[i].Enabled {
			continue
		}
		p := r.providers[i]
		after := ""
		if p.ID() == current.ProviderID {
			after = current.ProxyID
		}
		candidate, ok := p.Next(after, func(proxyID string) bool {
			ref := Ref{p.ID(), proxyID}
			return r.status(ref) == "healthy" && (!excludeCurrent || ref != current)
		})
		if ok {
			return Ref{p.ID(), candidate.ID}, true
		}
	}
	return Ref{}, false
}

func (r *Router) status(ref Ref) string {
	if status := r.health[ref]; status != "" {
		return status
	}
	return "unknown"
}

func validStatus(status string) bool {
	switch status {
	case "unknown", "healthy", "suspect", "unavailable":
		return true
	default:
		return false
	}
}

func (r *Router) saveChoice(ctx context.Context, ref Ref, reason string) error {
	next := r.selection
	next.ProviderID, next.ProxyID = ref.ProviderID, ref.ProxyID
	next.SelectedAt = time.Now().UTC()
	next.SwitchReason = reason
	if err := r.store.SaveSelection(ctx, next); err != nil {
		return err
	}
	r.selection = next
	return nil
}
