package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"isp/internal/config"
	"isp/internal/health"
	"isp/internal/routing"
	"isp/internal/state"
)

var errNotFound = errors.New("provider or proxy not found")
var errBadRequest = errors.New("invalid proxy request")

type App struct {
	Config  *config.File
	Router  *routing.Router
	Health  *health.Manager
	Store   *state.Store
	OnFatal func(error)
	mu      sync.Mutex
}

type selectionView struct {
	ProviderID   string `json:"provider_id"`
	ProxyID      string `json:"proxy_id"`
	AutoSwitch   bool   `json:"auto_switch"`
	SelectedAt   string `json:"selected_at"`
	SwitchReason string `json:"switch_reason"`
}

type proxyView struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Host                string `json:"host"`
	Port                int    `json:"port"`
	Enabled             bool   `json:"enabled"`
	Username            string `json:"username"`
	HasPassword         bool   `json:"has_password"`
	Status              string `json:"status"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastCheckedAt       string `json:"last_checked_at"`
	LastResult          string `json:"last_result"`
	LastError           string `json:"last_error"`
	NextCheckAt         string `json:"next_check_at"`
}

type providerView struct {
	ID      string      `json:"id"`
	Type    string      `json:"type"`
	Enabled bool        `json:"enabled"`
	Proxies []proxyView `json:"proxies"`
}

type stateView struct {
	ConfigRevision    string         `json:"config_revision"`
	SelectionRevision string         `json:"selection_revision"`
	Selection         selectionView  `json:"selection"`
	Providers         []providerView `json:"providers"`
}

type proxyInput struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	Enabled  bool    `json:"enabled"`
	Username string  `json:"username"`
	Password *string `json:"password"`
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", a.getState)
	mux.HandleFunc("PUT /api/auto-switch", a.setAutoSwitch)
	mux.HandleFunc("POST /api/rotate", a.rotate)
	mux.HandleFunc("PUT /api/selection", a.selectProxy)
	mux.HandleFunc("POST /api/providers/{provider}/proxies", a.createProxy)
	mux.HandleFunc("PUT /api/providers/{provider}/proxies/{proxy}", a.updateProxy)
	mux.HandleFunc("PATCH /api/providers/{provider}/proxies/{proxy}/enabled", a.setProxyEnabled)
	mux.HandleFunc("DELETE /api/providers/{provider}/proxies/{proxy}", a.deleteProxy)
	return mux
}

func (a *App) getState(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.writeState(w, r)
}

func (a *App) writeState(w http.ResponseWriter, r *http.Request) {
	cfg, configRevision := a.Config.SnapshotWithRevision()
	selection, selectionRevision := a.Router.SelectionWithRevision()
	records, err := a.Store.LoadHealth(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read health state")
		return
	}
	known := make(map[routing.Ref]state.Health)
	for _, record := range records {
		known[routing.Ref{ProviderID: record.ProviderID, ProxyID: record.ProxyID}] = record
	}
	response := stateView{
		ConfigRevision: configRevision, SelectionRevision: selectionRevision,
		Selection: selectionView{ProviderID: selection.ProviderID, ProxyID: selection.ProxyID, AutoSwitch: selection.AutoSwitch, SelectedAt: timeString(selection.SelectedAt), SwitchReason: selection.SwitchReason},
		Providers: make([]providerView, 0, len(cfg.Providers)),
	}
	for _, group := range cfg.Providers {
		provider := providerView{ID: group.ID, Type: group.Type, Enabled: group.Enabled, Proxies: make([]proxyView, 0, len(group.Proxies))}
		for _, proxy := range group.Proxies {
			health, found := known[routing.Ref{ProviderID: group.ID, ProxyID: proxy.ID}]
			status := "unknown"
			if found {
				status = health.Status
			}
			provider.Proxies = append(provider.Proxies, proxyView{
				ID: proxy.ID, Name: proxy.Name, Host: proxy.Host, Port: proxy.Port, Enabled: proxy.Enabled,
				Username: proxy.Username, HasPassword: proxy.Password != "", Status: status,
				ConsecutiveFailures: health.ConsecutiveFailures, LastCheckedAt: timeString(health.LastCheckedAt),
				LastResult: health.LastResult, LastError: health.LastError, NextCheckAt: timeString(health.NextCheckAt),
			})
		}
		response.Providers = append(response.Providers, provider)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (a *App) setAutoSwitch(w http.ResponseWriter, r *http.Request) {
	expected, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	if err := a.Router.SetAutoSwitchIfRevision(r.Context(), expected, *input.Enabled); err != nil {
		writeActionError(w, err)
		return
	}
	a.getState(w, r)
}

func (a *App) rotate(w http.ResponseWriter, r *http.Request) {
	expected, ok := requireRevision(w, r)
	if !ok {
		return
	}
	if err := a.Router.RotateIfRevision(r.Context(), expected); err != nil {
		writeActionError(w, err)
		return
	}
	a.getState(w, r)
}

func (a *App) selectProxy(w http.ResponseWriter, r *http.Request) {
	expected, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var input struct {
		ProviderID string `json:"provider_id"`
		ProxyID    string `json:"proxy_id"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := a.Router.SelectIfRevision(r.Context(), expected, routing.Ref{ProviderID: input.ProviderID, ProxyID: input.ProxyID}); err != nil {
		writeActionError(w, err)
		return
	}
	a.getState(w, r)
}

func (a *App) createProxy(w http.ResponseWriter, r *http.Request) {
	var input proxyInput
	if !decodeJSON(w, r, &input) {
		return
	}
	a.mutateConfig(w, r, func(cfg *config.Config) error {
		group := findProvider(cfg, r.PathValue("provider"))
		if group == nil {
			return errNotFound
		}
		for _, existing := range group.Proxies {
			if existing.ID == input.ID {
				return fmt.Errorf("%w: proxy ID already exists", errBadRequest)
			}
		}
		proxy := config.Proxy{ID: input.ID, Name: input.Name, Host: input.Host, Port: input.Port, Enabled: input.Enabled, Username: input.Username}
		if input.Password != nil {
			proxy.Password = *input.Password
		}
		group.Proxies = append(group.Proxies, proxy)
		return nil
	})
}

func (a *App) updateProxy(w http.ResponseWriter, r *http.Request) {
	var input proxyInput
	if !decodeJSON(w, r, &input) {
		return
	}
	a.mutateConfig(w, r, func(cfg *config.Config) error {
		proxy := findProxy(cfg, r.PathValue("provider"), r.PathValue("proxy"))
		if proxy == nil {
			return errNotFound
		}
		if input.ID != "" && input.ID != proxy.ID {
			return fmt.Errorf("%w: proxy ID cannot be changed", errBadRequest)
		}
		proxy.Name, proxy.Host, proxy.Port, proxy.Enabled, proxy.Username = input.Name, input.Host, input.Port, input.Enabled, input.Username
		if input.Password != nil {
			proxy.Password = *input.Password
		}
		return nil
	})
}

func (a *App) setProxyEnabled(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	a.mutateConfig(w, r, func(cfg *config.Config) error {
		proxy := findProxy(cfg, r.PathValue("provider"), r.PathValue("proxy"))
		if proxy == nil {
			return errNotFound
		}
		proxy.Enabled = *input.Enabled
		return nil
	})
}

func (a *App) deleteProxy(w http.ResponseWriter, r *http.Request) {
	a.mutateConfig(w, r, func(cfg *config.Config) error {
		group := findProvider(cfg, r.PathValue("provider"))
		if group == nil {
			return errNotFound
		}
		for i, proxy := range group.Proxies {
			if proxy.ID == r.PathValue("proxy") {
				group.Proxies = append(group.Proxies[:i], group.Proxies[i+1:]...)
				return nil
			}
		}
		return errNotFound
	})
}

func (a *App) mutateConfig(w http.ResponseWriter, r *http.Request, change func(*config.Config) error) {
	expected, ok := requireRevision(w, r)
	if !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.Config.UpdateIfRevision(expected, change)
	if err != nil {
		writeActionError(w, err)
		return
	}
	cfg := a.Config.Snapshot()
	routingErr := a.Router.Reconfigure(r.Context(), cfg)
	healthErr := a.Health.Reconfigure(r.Context(), cfg)
	if routingErr != nil || healthErr != nil {
		cause := errors.Join(routingErr, healthErr)
		if a.OnFatal != nil {
			a.OnFatal(cause)
		}
		writeError(w, http.StatusInternalServerError, "configuration saved, but runtime update failed; restart required")
		return
	}
	a.writeState(w, r)
}

func findProvider(cfg *config.Config, id string) *config.Provider {
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == id {
			return &cfg.Providers[i]
		}
	}
	return nil
}

func findProxy(cfg *config.Config, providerID, proxyID string) *config.Proxy {
	group := findProvider(cfg, providerID)
	if group == nil {
		return nil
	}
	for i := range group.Proxies {
		if group.Proxies[i].ID == proxyID {
			return &group.Proxies[i]
		}
	}
	return nil
}

func requireRevision(w http.ResponseWriter, r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("If-Match"))
	if value == "" {
		writeError(w, http.StatusPreconditionRequired, "If-Match revision is required")
		return "", false
	}
	if len(value) != 66 || value[0] != '"' || value[len(value)-1] != '"' {
		writeError(w, http.StatusBadRequest, "invalid If-Match revision")
		return "", false
	}
	return value[1 : len(value)-1], true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "application/json is required")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON value")
		return false
	}
	return true
}

func writeActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, config.ErrConflict), errors.Is(err, routing.ErrConflict):
		writeError(w, http.StatusConflict, "state changed; refresh and retry")
	case errors.Is(err, errNotFound), errors.Is(err, routing.ErrNotEnabled):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, routing.ErrNoAlternative), errors.Is(err, routing.ErrNoAvailable):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, config.ErrInvalid), errors.Is(err, errBadRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func timeString(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
