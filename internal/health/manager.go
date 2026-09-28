package health

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"isp/internal/config"
	"isp/internal/routing"
	"isp/internal/state"
)

type Store interface {
	LoadHealth(context.Context) ([]state.Health, error)
	SaveHealth(context.Context, state.Health) error
}

type Notifier interface {
	HealthChanged(context.Context, routing.Ref, string) error
}

type entry struct {
	proxy    config.Proxy
	health   state.Health
	checking bool
}

type Manager struct {
	// OnError receives store and routing failures. They never stop Run, so a
	// full disk or locked database does not take the proxy entries down.
	OnError  func(error)
	mu       sync.Mutex
	settings config.HealthCheck
	url      string
	store    Store
	notifier Notifier
	probe    Probe
	entries  map[routing.Ref]*entry
	wake     chan struct{}
}

func New(ctx context.Context, cfg config.Config, store Store, notifier Notifier, probe Probe) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil || notifier == nil {
		return nil, errors.New("health store and routing notifier are required")
	}
	if probe == nil {
		probe = HTTPProbe
	}
	records, err := store.LoadHealth(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[routing.Ref]state.Health)
	for _, record := range records {
		known[routing.Ref{ProviderID: record.ProviderID, ProxyID: record.ProxyID}] = record
	}
	m := &Manager{settings: cfg.HealthCheck, url: cfg.HealthCheck.URL, store: store, notifier: notifier, probe: probe, entries: make(map[routing.Ref]*entry), wake: make(chan struct{}, 1)}
	for _, group := range cfg.Providers {
		if !group.Enabled {
			continue
		}
		for _, proxy := range group.Proxies {
			if !proxy.Enabled {
				continue
			}
			ref := routing.Ref{ProviderID: group.ID, ProxyID: proxy.ID}
			health, found := known[ref]
			if !found {
				health = state.Health{ProviderID: ref.ProviderID, ProxyID: ref.ProxyID, Status: "unknown"}
			}
			m.entries[ref] = &entry{proxy: proxy, health: health}
		}
	}
	return m, nil
}

// Run checks each enabled proxy independently until ctx is canceled. Saved
// next-check times survive restarts; a missing time means the proxy is due
// immediately.
func (m *Manager) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	var checks sync.WaitGroup
	defer func() {
		cancel()
		checks.Wait()
	}()
	slots := make(chan struct{}, 16)
	for {
		now := time.Now().UTC()
		var due []struct {
			ref      routing.Ref
			item     *entry
			settings config.HealthCheck
			url      string
		}
		var next time.Time
		m.mu.Lock()
		for ref, item := range m.entries {
			if item.checking {
				continue
			}
			if item.health.NextCheckAt.IsZero() || !item.health.NextCheckAt.After(now) {
				item.checking = true
				due = append(due, struct {
					ref      routing.Ref
					item     *entry
					settings config.HealthCheck
					url      string
				}{ref, item, m.settings, m.url})
			} else if next.IsZero() || item.health.NextCheckAt.Before(next) {
				next = item.health.NextCheckAt
			}
		}
		m.mu.Unlock()
		for _, work := range due {
			checks.Add(1)
			go func(ref routing.Ref, item *entry, settings config.HealthCheck, url string) {
				defer checks.Done()
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					return
				}
				m.check(ctx, ref, item, settings, url)
			}(work.ref, work.item, work.settings, work.url)
		}
		var timer *time.Timer
		var timerC <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(time.Until(next))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil
		case <-m.wake:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (m *Manager) check(ctx context.Context, ref routing.Ref, item *entry, settings config.HealthCheck, url string) {
	probeCtx, cancel := context.WithTimeout(ctx, settings.Timeout.Value())
	err := m.probe(probeCtx, item.proxy, url)
	cancel()
	if ctx.Err() != nil {
		return
	}
	now := time.Now().UTC()
	m.mu.Lock()
	if m.entries[ref] != item {
		m.mu.Unlock()
		return
	}
	next := afterProbe(item.health, err, now, settings, rand.Float64())
	// An unsaved result still drives routing; the next check saves again.
	saveErr := m.store.SaveHealth(ctx, next)
	item.health = next
	item.checking = false
	if ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	// Keep a reconfiguration from accepting this result between the store write
	// and the router notification.
	notifyErr := m.notifier.HealthChanged(ctx, ref, next.Status)
	m.mu.Unlock()
	m.signal()
	if ctx.Err() != nil {
		return
	}
	if saveErr != nil {
		m.report(fmt.Errorf("save health for %s/%s: %w", ref.ProviderID, ref.ProxyID, saveErr))
	}
	if notifyErr != nil && !errors.Is(notifyErr, routing.ErrNoAvailable) {
		m.report(fmt.Errorf("notify routing for %s/%s: %w", ref.ProviderID, ref.ProxyID, notifyErr))
	}
}

func (m *Manager) report(err error) {
	if m.OnError != nil {
		m.OnError(err)
	}
}

// Reconfigure replaces the active proxy set after YAML and routing have been
// updated. Results from checks started against the old set are discarded.
func (m *Manager) Reconfigure(ctx context.Context, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.store.LoadHealth(ctx)
	if err != nil {
		return err
	}
	known := make(map[routing.Ref]state.Health)
	for _, record := range records {
		known[routing.Ref{ProviderID: record.ProviderID, ProxyID: record.ProxyID}] = record
	}
	next := make(map[routing.Ref]*entry)
	for _, group := range cfg.Providers {
		if !group.Enabled {
			continue
		}
		for _, proxy := range group.Proxies {
			if !proxy.Enabled {
				continue
			}
			ref := routing.Ref{ProviderID: group.ID, ProxyID: proxy.ID}
			health, found := known[ref]
			if !found {
				health = state.Health{ProviderID: ref.ProviderID, ProxyID: ref.ProxyID, Status: "unknown"}
			}
			next[ref] = &entry{proxy: proxy, health: health}
		}
	}
	m.entries = next
	m.settings = cfg.HealthCheck
	m.url = cfg.HealthCheck.URL
	m.signal()
	return nil
}

// ReportConnectionError moves the next check forward without treating one
// client connection error as an independent health-check failure.
func (m *Manager) ReportConnectionError(ctx context.Context, ref routing.Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	item := m.entries[ref]
	if item == nil || item.checking {
		return nil
	}
	due := time.Now().UTC()
	if recent := item.health.LastCheckedAt.Add(5 * time.Second); recent.After(due) {
		due = recent
	}
	if !item.health.NextCheckAt.IsZero() && !due.Before(item.health.NextCheckAt) {
		return nil
	}
	next := item.health
	next.NextCheckAt = due
	if err := m.store.SaveHealth(ctx, next); err != nil {
		return err
	}
	item.health = next
	m.signal()
	return nil
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func afterProbe(previous state.Health, probeErr error, now time.Time, settings config.HealthCheck, sample float64) state.Health {
	next := previous
	next.LastCheckedAt = now
	if probeErr == nil {
		next.Status = "healthy"
		next.ConsecutiveSuccesses++
		next.ConsecutiveFailures = 0
		next.BackoffStage = 0
		next.LastResult = "success"
		next.LastError = ""
		span := settings.IntervalMax.Value() - settings.IntervalMin.Value()
		next.NextCheckAt = now.Add(settings.IntervalMin.Value() + time.Duration(float64(span)*sample))
		return next
	}
	next.ConsecutiveSuccesses = 0
	next.ConsecutiveFailures++
	if next.BackoffStage < 32 {
		next.BackoffStage++
	}
	next.LastResult = "failure"
	next.LastError = probeErr.Error()
	if len(next.LastError) > 500 {
		next.LastError = next.LastError[:500]
	}
	if next.ConsecutiveFailures >= settings.FailureThreshold {
		next.Status = "unavailable"
	} else {
		next.Status = "suspect"
	}
	delay := settings.IntervalMin.Value()
	limit := settings.BackoffMax.Value()
	for i := 1; i < next.BackoffStage && delay < limit; i++ {
		if delay > limit/2 {
			delay = limit
		} else {
			delay *= 2
		}
	}
	delay = time.Duration(float64(delay) * (0.8 + 0.4*sample))
	if delay > limit {
		delay = limit
	}
	next.NextCheckAt = now.Add(delay)
	return next
}
