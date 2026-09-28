package provider

import (
	"testing"

	"isp/internal/config"
)

func TestProxySellerCyclesInConfigurationOrder(t *testing.T) {
	p := NewProxySeller(config.Provider{ID: "seller", Proxies: []config.Proxy{
		{ID: "a", Enabled: true}, {ID: "b", Enabled: false}, {ID: "c", Enabled: true},
	}})
	eligible := func(id string) bool { return id != "b" }
	for _, tc := range []struct{ after, want string }{{"", "a"}, {"a", "c"}, {"c", "a"}, {"removed", "a"}} {
		got, ok := p.Next(tc.after, eligible)
		if !ok || got.ID != tc.want {
			t.Fatalf("after %q: got %q, %v; want %q", tc.after, got.ID, ok, tc.want)
		}
	}
	if _, ok := p.Next("a", func(string) bool { return false }); ok {
		t.Fatal("expected no candidate")
	}
}
