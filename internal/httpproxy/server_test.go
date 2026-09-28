package httpproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"isp/internal/config"
	"isp/internal/routing"
)

type testSelector struct {
	mu       sync.RWMutex
	snapshot routing.Snapshot
	ready    bool
}

func (s *testSelector) Snapshot() (routing.Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot, s.ready
}

func (s *testSelector) set(address, username, password string) {
	host, portText, _ := net.SplitHostPort(address)
	port, _ := strconv.Atoi(portText)
	s.mu.Lock()
	s.snapshot = routing.Snapshot{Ref: routing.Ref{ProviderID: "http-seller", ProxyID: address}, Proxy: config.Proxy{Host: host, Port: port, Username: username, Password: password}}
	s.ready = true
	s.mu.Unlock()
}

func startHTTPUpstream(t *testing.T, destination, username, password string, accepts *atomic.Int32) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepts.Add(1)
		if username != "" {
			wanted := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
			if r.Header.Get("Proxy-Authorization") != wanted {
				w.WriteHeader(http.StatusProxyAuthRequired)
				return
			}
		}
		if r.Method == http.MethodConnect {
			if r.Host != "target.invalid:443" {
				t.Errorf("CONNECT target %q", r.Host)
			}
			target, err := net.Dial("tcp", destination)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			defer target.Close()
			client, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer client.Close()
			_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = buffered.Flush()
			go func() { _, _ = io.Copy(target, buffered); target.Close() }()
			_, _ = io.Copy(client, target)
			return
		}
		if r.Host != "target.invalid" {
			t.Errorf("forward target %q", r.Host)
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.Header = r.Header.Clone()
		out.Header.Del("Proxy-Authorization")
		transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return net.Dial("tcp", destination)
		}}
		defer transport.CloseIdleConnections()
		response, err := transport.RoundTrip(out)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func startGateway(t *testing.T, h *Handler) string {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(func() { h.Close(); server.Close() })
	return strings.TrimPrefix(server.URL, "http://")
}

func TestHTTPForwardThroughHTTPUpstream(t *testing.T) {
	for _, tc := range []struct{ name, user, pass string }{{"no-auth", "", ""}, {"user-password", "alice", "secret"}} {
		t.Run(tc.name, func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "target.invalid" || r.Header.Get("X-Test") != "yes" || r.Header.Get("Proxy-Authorization") != "" {
					t.Errorf("wrong forwarded request: host=%q headers=%v", r.Host, r.Header)
				}
				_, _ = w.Write([]byte("via-http-upstream"))
			}))
			defer target.Close()
			var accepts atomic.Int32
			upstream := startHTTPUpstream(t, target.Listener.Addr().String(), tc.user, tc.pass, &accepts)
			selector := &testSelector{}
			selector.set(upstream, tc.user, tc.pass)
			gateway := startGateway(t, &Handler{Selector: selector})
			proxyURL, _ := url.Parse("http://" + gateway)
			client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 3 * time.Second}
			request, _ := http.NewRequest(http.MethodGet, "http://target.invalid/path", nil)
			request.Header.Set("X-Test", "yes")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusOK || string(body) != "via-http-upstream" || accepts.Load() != 1 {
				t.Fatalf("response: status=%d body=%q upstream accepts=%d", response.StatusCode, body, accepts.Load())
			}
			if tc.user != "" {
				selector.set(upstream, tc.user, "wrong")
				response, err := client.Get("http://target.invalid/path")
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusBadGateway {
					t.Fatalf("wrong upstream credentials: status=%d", response.StatusCode)
				}
			}
		})
	}
}

func TestCONNECTPinsHTTPUpstreamAndCarriesBufferedBytes(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	var first, second atomic.Int32
	upstreamOne := startHTTPUpstream(t, echo.Addr().String(), "", "", &first)
	upstreamTwo := startHTTPUpstream(t, echo.Addr().String(), "alice", "secret", &second)
	selector := &testSelector{}
	selector.set(upstreamOne, "", "")
	handler := &Handler{Selector: selector}
	gateway := startGateway(t, handler)
	conn, err := net.Dial("tcp", gateway)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(conn, "CONNECT target.invalid:443 HTTP/1.1\r\nHost: target.invalid:443\r\n\r\nfirst")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT response: %+v, %v", response, err)
	}
	data := make([]byte, 5)
	if _, err := io.ReadFull(reader, data); err != nil || string(data) != "first" {
		t.Fatalf("buffered payload: %q, %v", data, err)
	}
	selector.set(upstreamTwo, "alice", "secret")
	_, _ = io.WriteString(conn, "again")
	if _, err := io.ReadFull(reader, data); err != nil || string(data) != "again" {
		t.Fatalf("old tunnel after switch: %q, %v", data, err)
	}
	other, err := net.Dial("tcp", gateway)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_ = other.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(other, "CONNECT target.invalid:443 HTTP/1.1\r\nHost: target.invalid:443\r\n\r\nnext")
	otherReader := bufio.NewReader(other)
	response, err = http.ReadResponse(otherReader, nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("second CONNECT response: %+v, %v", response, err)
	}
	data = make([]byte, 4)
	if _, err := io.ReadFull(otherReader, data); err != nil || string(data) != "next" {
		t.Fatalf("new tunnel payload: %q, %v", data, err)
	}
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("tunnels used wrong upstream: first=%d second=%d", first.Load(), second.Load())
	}
	selector.set(upstreamTwo, "alice", "wrong")
	failed, err := net.Dial("tcp", gateway)
	if err != nil {
		t.Fatal(err)
	}
	defer failed.Close()
	_ = failed.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(failed, "CONNECT target.invalid:443 HTTP/1.1\r\nHost: target.invalid:443\r\n\r\n")
	denied, err := http.ReadResponse(bufio.NewReader(failed), nil)
	if err != nil || denied.StatusCode != http.StatusBadGateway {
		t.Fatalf("wrong HTTP upstream credentials: %+v, %v", denied, err)
	}
	denied.Body.Close()
	handler.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("tunnel remained open after handler shutdown")
	}
}

func TestHTTPSClientThroughHTTPUpstream(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secure-through-http"))
	}))
	defer target.Close()
	var accepts atomic.Int32
	upstream := startHTTPUpstream(t, target.Listener.Addr().String(), "alice", "secret", &accepts)
	selector := &testSelector{}
	selector.set(upstream, "alice", "secret")
	gateway := startGateway(t, &Handler{Selector: selector})
	proxyURL, _ := url.Parse("http://" + gateway)
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Test TLS server uses a self-signed certificate.
	}, Timeout: 3 * time.Second}
	response, err := client.Get("https://target.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "secure-through-http" || accepts.Load() != 1 {
		t.Fatalf("HTTPS response: status=%d body=%q upstream accepts=%d", response.StatusCode, body, accepts.Load())
	}
}

func TestNoSelectedProxyNeverConnectsDirectly(t *testing.T) {
	selector := &testSelector{}
	gateway := startGateway(t, &Handler{Selector: selector})
	proxyURL, _ := url.Parse("http://" + gateway)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: time.Second}
	response, err := client.Get("http://target.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", response.StatusCode)
	}
	selector.set("127.0.0.1:1", "", "")
	response, err = client.Get("http://target.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream failure should be 502, got %d", response.StatusCode)
	}
}

func TestOneWayTunnelTrafficKeepsCONNECTOpen(t *testing.T) {
	destination := startStreamingTarget(t, 12, 50*time.Millisecond)
	var accepts atomic.Int32
	upstream := startHTTPUpstream(t, destination, "", "", &accepts)
	selector := &testSelector{}
	selector.set(upstream, "", "")
	gateway := startGateway(t, &Handler{Selector: selector, IdleTimeout: 200 * time.Millisecond})
	conn, err := net.Dial("tcp", gateway)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, "CONNECT target.invalid:443 HTTP/1.1\r\nHost: target.invalid:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT response %v, %v", response, err)
	}
	// The client stays silent for longer than the idle timeout while the
	// target keeps sending, so the tunnel must stay open.
	received, err := io.ReadAll(reader)
	if err != nil || len(received) != 12 {
		t.Fatalf("received %d of 12 bytes, %v", len(received), err)
	}
}

func startStreamingTarget(t *testing.T, chunks int, interval time.Duration) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for range chunks {
			if _, err := conn.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(interval)
		}
	}()
	return listener.Addr().String()
}
