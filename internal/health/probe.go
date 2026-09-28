package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"isp/internal/config"
	"isp/internal/socks5"
)

type Probe func(context.Context, config.Proxy, string) error

// HTTPProbe counts any complete HTTP response as success, regardless of its
// status code. The target hostname is sent through the selected SOCKS5 proxy.
func HTTPProbe(ctx context.Context, proxy config.Proxy, rawURL string) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			return socks5.DialContext(ctx, proxy, address)
		},
	}
	return runProbe(ctx, transport, rawURL, false)
}

// HTTPUpstreamProbe checks a target through an HTTP forward proxy. HTTPS
// targets are tunneled with CONNECT by net/http.
func HTTPUpstreamProbe(ctx context.Context, proxy config.Proxy, rawURL string) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	proxyURL := &url.URL{Scheme: "http", Host: net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port))}
	if proxy.Username != "" {
		proxyURL.User = url.UserPassword(proxy.Username, proxy.Password)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	return runProbe(ctx, transport, rawURL, true)
}

func runProbe(ctx context.Context, transport *http.Transport, rawURL string, rejectProxyAuth bool) error {
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if rejectProxyAuth && response.StatusCode == http.StatusProxyAuthRequired {
		return errors.New("HTTP upstream rejected proxy authentication")
	}
	return nil
}
