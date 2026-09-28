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

// Run checks each enabled proxy independently. Saved next-check times survive
// restarts; a missing time means the proxy is due immediately.
func (m *Manager) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	var checks sync.WaitGroup
	defer func() {
		cancel()
		checks.Wait()
	}()
	slots := make(chan struct{}, 16)
	failures := make(chan error, 1)
	for {
		now := time.Now().UTC()
		var due []struct {
			ref   routing.Ref
			proxy config.Proxy
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
					ref   routing.Ref
					proxy config.Proxy
				}{ref, item.proxy})
			} else if next.IsZero() || item.health.NextCheckAt.Before(next) {
				next = item.health.NextCheckAt
			}
		}
		m.mu.Unlock()
		for _, work := range due {
			checks.Add(1)
			go func(ref routing.Ref, proxy config.Proxy) {
				defer checks.Done()
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					return
				}
				if err := m.check(ctx, ref, proxy); err != nil {
					select {
					case failures <- err:
					default:
					}
				}
			}(work.ref, work.proxy)
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
		case err := <-failures:
			if timer != nil {
				timer.Stop()
			}
			return err
		case <-m.wake:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (m *Manager) check(ctx context.Context, ref routing.Ref, proxy config.Proxy) error {
	probeCtx, cancel := context.WithTimeout(ctx, m.settings.Timeout.Value())
	err := m.probe(probeCtx, proxy, m.url)
	cancel()
	if ctx.Err() != nil {
		return nil
	}
	now := time.Now().UTC()
	m.mu.Lock()
	item := m.entries[ref]
	next := afterProbe(item.health, err, now, m.settings, rand.Float64())
	if saveErr := m.store.SaveHealth(ctx, next); saveErr != nil {
		item.checking = false
		m.mu.Unlock()
		m.signal()
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("save health for %s/%s: %w", ref.ProviderID, ref.ProxyID, saveErr)
	}
	item.health = next
	item.checking = false
	m.mu.Unlock()
	m.signal()
	if ctx.Err() != nil {
		return nil
	}
	if notifyErr := m.notifier.HealthChanged(ctx, ref, next.Status); notifyErr != nil && !errors.Is(notifyErr, routing.ErrNoAvailable) {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("notify routing for %s/%s: %w", ref.ProviderID, ref.ProxyID, notifyErr)
	}
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
