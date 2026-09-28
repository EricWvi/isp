package provider

import "isp/internal/config"

// Provider owns the ordering and selection of its proxies. The router decides
// which providers to visit and supplies the current eligibility predicate.
type Provider interface {
	ID() string
	DisplayName() string
	Next(afterProxyID string, eligible func(proxyID string) bool) (config.Proxy, bool)
}

// ProxySeller uses the order of the YAML proxy list. A retained proxy ID keeps
// its place in the cycle even when its host, port or credentials change.
type ProxySeller struct {
	id      string
	proxies []config.Proxy
}

func NewProxySeller(cfg config.Provider) *ProxySeller {
	return &ProxySeller{id: cfg.ID, proxies: append([]config.Proxy(nil), cfg.Proxies...)}
}

func (p *ProxySeller) ID() string          { return p.id }
func (p *ProxySeller) DisplayName() string { return "Proxy Seller" }

func (p *ProxySeller) Next(afterProxyID string, eligible func(string) bool) (config.Proxy, bool) {
	start := 0
	for i, proxy := range p.proxies {
		if proxy.ID == afterProxyID {
			start = (i + 1) % len(p.proxies)
			break
		}
	}
	for offset := 0; offset < len(p.proxies); offset++ {
		proxy := p.proxies[(start+offset)%len(p.proxies)]
		if proxy.Enabled && eligible(proxy.ID) {
			return proxy, true
		}
	}
	return config.Proxy{}, false
}
