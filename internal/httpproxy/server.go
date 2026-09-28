package httpproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"isp/internal/config"
	"isp/internal/routing"
	"isp/internal/tunnel"
)

type Selector interface {
	Snapshot() (routing.Snapshot, bool)
}

// Handler is a local HTTP forward proxy backed exclusively by the selected
// HTTP upstream. Each request or CONNECT tunnel pins one routing snapshot.
type Handler struct {
	Selector        Selector
	DialTimeout     time.Duration
	IdleTimeout     time.Duration
	OnUpstreamError func(routing.Ref, error)
	mu              sync.Mutex
	tunnels         map[net.Conn]net.Conn
	closed          bool
}

// Server returns an HTTP server for h. IdleTimeout also bounds how long a
// keep-alive client connection may wait between requests.
func (h *Handler) Server() *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       durationOr(h.IdleTimeout, 5*time.Minute),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Selector == nil {
		http.Error(w, "proxy unavailable", http.StatusBadGateway)
		return
	}
	snapshot, ok := h.Selector.Snapshot()
	if !ok {
		http.Error(w, "no proxy selected", http.StatusBadGateway)
		return
	}
	if r.Method == http.MethodConnect {
		h.connect(w, r, snapshot)
		return
	}
	if r.URL == nil || !r.URL.IsAbs() || r.URL.Scheme != "http" || r.URL.Hostname() == "" {
		http.Error(w, "absolute HTTP URL required", http.StatusBadRequest)
		return
	}
	h.forward(w, r, snapshot)
}

func (h *Handler) dial(ctx context.Context, snapshot routing.Snapshot, target string) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, durationOr(h.DialTimeout, 10*time.Second))
	defer cancel()
	address := net.JoinHostPort(snapshot.Proxy.Host, strconv.Itoa(snapshot.Proxy.Port))
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	if err != nil && ctx.Err() == nil && h.OnUpstreamError != nil {
		h.OnUpstreamError(snapshot.Ref, err)
	}
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(durationOr(h.DialTimeout, 10*time.Second))
	if ctxDeadline, ok := dialCtx.Deadline(); ok {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if snapshot.Proxy.Username != "" {
		credentials := base64.StdEncoding.EncodeToString([]byte(snapshot.Proxy.Username + ":" + snapshot.Proxy.Password))
		request += "Proxy-Authorization: Basic " + credentials + "\r\n"
	}
	if _, err = io.WriteString(conn, request+"\r\n"); err == nil {
		var response *http.Response
		reader := bufio.NewReader(conn)
		response, err = http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
		if err == nil {
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("HTTP upstream CONNECT returned %s", response.Status)
			} else {
				_ = conn.SetDeadline(time.Time{})
				return &bufferedConn{Conn: conn, reader: reader}, nil
			}
		}
	}
	conn.Close()
	if ctx.Err() == nil && h.OnUpstreamError != nil {
		h.OnUpstreamError(snapshot.Ref, err)
	}
	return nil, err
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func proxyURL(proxy config.Proxy) *url.URL {
	u := &url.URL{Scheme: "http", Host: net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port))}
	if proxy.Username != "" {
		u.User = url.UserPassword(proxy.Username, proxy.Password)
	}
	return u
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request, snapshot routing.Snapshot) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		http.Error(w, "CONNECT target must be host:port", http.StatusBadRequest)
		return
	}
	upstream, err := h.dial(r.Context(), snapshot, target)
	if err != nil {
		http.Error(w, "upstream connection failed", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "CONNECT unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	defer client.Close()
	defer upstream.Close()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if h.tunnels == nil {
		h.tunnels = make(map[net.Conn]net.Conn)
	}
	h.tunnels[client] = upstream
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.tunnels, client)
		h.mu.Unlock()
	}()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	tunnel.Relay(r.Context(), client, buffered, upstream, durationOr(h.IdleTimeout, 5*time.Minute))
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, snapshot routing.Snapshot) {
	transport := &http.Transport{
		Proxy:                 http.ProxyURL(proxyURL(snapshot.Proxy)),
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: durationOr(h.DialTimeout, 10*time.Second),
	}
	defer transport.CloseIdleConnections()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header = r.Header.Clone()
	stripHopHeaders(out.Header)
	out.Header.Del("Proxy-Authorization")
	response, err := transport.RoundTrip(out)
	if err != nil {
		if r.Context().Err() == nil && h.OnUpstreamError != nil {
			h.OnUpstreamError(snapshot.Ref, err)
		}
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusProxyAuthRequired {
		if h.OnUpstreamError != nil {
			h.OnUpstreamError(snapshot.Ref, errors.New("HTTP upstream rejected proxy authentication"))
		}
		http.Error(w, "upstream authentication failed", http.StatusBadGateway)
		return
	}
	stripHopHeaders(response.Header)
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (h *Handler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for client, upstream := range h.tunnels {
		_ = client.Close()
		_ = upstream.Close()
	}
}

func stripHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, field := range strings.Split(value, ",") {
			header.Del(textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(field)))
		}
	}
	for _, field := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(field)
	}
}

func durationOr(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}
