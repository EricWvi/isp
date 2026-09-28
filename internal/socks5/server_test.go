package socks5

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"isp/internal/config"
	"isp/internal/routing"
	"isp/internal/state"
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

func (s *testSelector) set(addr, username, password string) {
	host, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)
	s.mu.Lock()
	s.snapshot = routing.Snapshot{Ref: routing.Ref{ProviderID: "seller", ProxyID: addr}, Proxy: config.Proxy{Host: host, Port: port, Username: username, Password: password}}
	s.ready = true
	s.mu.Unlock()
}

func startGateway(t *testing.T, s *Server) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.Serve(ctx, listener)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return listener.Addr().String(), cancel, done
}

func startTarget(t *testing.T, waitForEOF bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if waitForEOF {
					io.Copy(io.Discard, conn)
					conn.Write([]byte("done"))
					return
				}
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func startMockProxy(t *testing.T, target, username, password string, accepts *atomic.Int32) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				greeting := make([]byte, 3)
				if _, err := io.ReadFull(conn, greeting); err != nil || greeting[0] != 5 || greeting[1] != 1 {
					return
				}
				method := byte(noAuth)
				if username != "" {
					method = userPassword
				}
				if greeting[2] != method {
					return
				}
				conn.Write([]byte{5, method})
				if method == userPassword {
					header := make([]byte, 2)
					if _, err := io.ReadFull(conn, header); err != nil || header[0] != 1 {
						return
					}
					user := make([]byte, int(header[1]))
					if _, err := io.ReadFull(conn, user); err != nil {
						return
					}
					length := make([]byte, 1)
					if _, err := io.ReadFull(conn, length); err != nil {
						return
					}
					pass := make([]byte, int(length[0]))
					if _, err := io.ReadFull(conn, pass); err != nil || string(user) != username || string(pass) != password {
						conn.Write([]byte{1, 1})
						return
					}
					conn.Write([]byte{1, 0})
				}
				if _, _, err := readRequest(conn); err != nil {
					return
				}
				destination, err := net.Dial("tcp", target)
				if err != nil {
					writeFailure(conn, 5)
					return
				}
				defer destination.Close()
				conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				conn.SetDeadline(time.Time{})
				relay(context.Background(), conn, destination, 5*time.Second)
			}()
		}
	}()
	return listener.Addr().String()
}

func connectClient(t *testing.T, gateway string, command byte) (*net.TCPConn, byte) {
	t.Helper()
	// The fake upstream maps this domain to the local target. The gateway must
	// pass the domain through without resolving or dialing it itself.
	domain := []byte("target.invalid")
	request := append([]byte{5, command, 0, 3, byte(len(domain))}, domain...)
	request = append(request, 0, 80)
	return connectWithRequest(t, gateway, request)
}

func connectWithRequest(t *testing.T, gateway string, request []byte) (*net.TCPConn, byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", gateway, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := conn.(*net.TCPConn)
	client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(client, method); err != nil || method[0] != 5 || method[1] != 0 {
		t.Fatalf("client greeting: %v, %v", method, err)
	}
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	reply, err := readReply(client)
	if err != nil {
		t.Fatal(err)
	}
	client.SetDeadline(time.Time{})
	return client, reply[1]
}

func roundTrip(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, response); err != nil || string(response) != payload {
		t.Fatalf("round trip: %q, %v", response, err)
	}
	conn.SetDeadline(time.Time{})
}

func TestTCPConnectThroughBothUpstreamAuthModes(t *testing.T) {
	for _, tc := range []struct{ name, user, pass string }{{"no-auth", "", ""}, {"user-password", "alice", "secret"}} {
		t.Run(tc.name, func(t *testing.T) {
			target := startTarget(t, false)
			var accepts atomic.Int32
			upstream := startMockProxy(t, target, tc.user, tc.pass, &accepts)
			selector := &testSelector{}
			selector.set(upstream, tc.user, tc.pass)
			gateway, _, _ := startGateway(t, &Server{Selector: selector})
			client, status := connectClient(t, gateway, 1)
			defer client.Close()
			if status != 0 {
				t.Fatalf("CONNECT failed with status %d", status)
			}
			roundTrip(t, client, "hello")
			if accepts.Load() != 1 {
				t.Fatalf("upstream accepts: %d", accepts.Load())
			}
		})
	}
}

func TestCurlSOCKS5HostnameCompatibility(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("through-proxy"))
	}))
	defer target.Close()
	for _, tc := range []struct{ name, user, pass string }{{"no-auth", "", ""}, {"user-password", "alice", "secret"}} {
		t.Run(tc.name, func(t *testing.T) {
			var accepts atomic.Int32
			upstream := startMockProxy(t, target.Listener.Addr().String(), tc.user, tc.pass, &accepts)
			selector := &testSelector{}
			selector.set(upstream, tc.user, tc.pass)
			gateway, _, _ := startGateway(t, &Server{Selector: selector})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "curl", "--silent", "--show-error", "--fail", "--noproxy", "", "--socks5-hostname", gateway, "http://target.invalid/")
			output, err := cmd.CombinedOutput()
			if err != nil || string(output) != "through-proxy" || accepts.Load() != 1 {
				t.Fatalf("curl via gateway: %q, %v, accepts=%d", output, err, accepts.Load())
			}
		})
	}
}

func TestSwitchOnlyAffectsNewConnections(t *testing.T) {
	target := startTarget(t, false)
	var firstAccepts, secondAccepts atomic.Int32
	first := startMockProxy(t, target, "", "", &firstAccepts)
	second := startMockProxy(t, target, "", "", &secondAccepts)
	makeProxy := func(id, address string) config.Proxy {
		host, portText, _ := net.SplitHostPort(address)
		port, _ := strconv.Atoi(portText)
		return config.Proxy{ID: id, Host: host, Port: port, Enabled: true}
	}
	cfg := config.Defaults()
	cfg.Providers = []config.Provider{{ID: "seller", Type: "proxy-seller", Enabled: true, Proxies: []config.Proxy{makeProxy("first", first), makeProxy("second", second)}}}
	store, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	router, err := routing.Open(context.Background(), cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Select(context.Background(), routing.Ref{ProviderID: "seller", ProxyID: "first"}); err != nil {
		t.Fatal(err)
	}
	gateway, _, _ := startGateway(t, &Server{Selector: router})
	oldConn, status := connectClient(t, gateway, 1)
	defer oldConn.Close()
	if status != 0 {
		t.Fatalf("old CONNECT status %d", status)
	}
	if err := router.Select(context.Background(), routing.Ref{ProviderID: "seller", ProxyID: "second"}); err != nil {
		t.Fatal(err)
	}
	newConn, status := connectClient(t, gateway, 1)
	defer newConn.Close()
	if status != 0 {
		t.Fatalf("new CONNECT status %d", status)
	}
	roundTrip(t, oldConn, "old survives")
	roundTrip(t, newConn, "new route")
	if firstAccepts.Load() != 1 || secondAccepts.Load() != 1 {
		t.Fatalf("upstream selection counts: %d, %d", firstAccepts.Load(), secondAccepts.Load())
	}
}

func TestHalfCloseAndCancellation(t *testing.T) {
	target := startTarget(t, true)
	var accepts atomic.Int32
	upstream := startMockProxy(t, target, "", "", &accepts)
	selector := &testSelector{}
	selector.set(upstream, "", "")
	gateway, cancel, done := startGateway(t, &Server{Selector: selector})
	client, status := connectClient(t, gateway, 1)
	if status != 0 {
		t.Fatalf("CONNECT status %d", status)
	}
	client.Write([]byte("payload"))
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	response, err := io.ReadAll(client)
	if err != nil || string(response) != "done" {
		t.Fatalf("half-close response %q, %v", response, err)
	}
	client.Close()
	second, status := connectClient(t, gateway, 1)
	if status != 0 {
		t.Fatalf("second CONNECT status %d", status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
	second.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection remained open after cancellation")
	}
	second.Close()
}

func TestNoProxyNeverDialsDirectForTCPOrUDP(t *testing.T) {
	selector := &testSelector{}
	gateway, _, _ := startGateway(t, &Server{Selector: selector})
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := target.Accept()
		if err == nil {
			conn.Close()
			accepted <- struct{}{}
		}
	}()
	ip := net.ParseIP("127.0.0.1").To4()
	port := target.Addr().(*net.TCPAddr).Port
	request := []byte{5, 1, 0, 1, ip[0], ip[1], ip[2], ip[3], byte(port >> 8), byte(port)}
	client, status := connectWithRequest(t, gateway, request)
	client.Close()
	if status != 1 {
		t.Fatalf("missing proxy status %d", status)
	}
	select {
	case <-accepted:
		t.Fatal("gateway dialed the target directly")
	case <-time.After(100 * time.Millisecond):
	}
	client, status = connectClient(t, gateway, 3)
	client.Close()
	if status != 1 {
		t.Fatalf("UDP request status %d", status)
	}
}

func TestUpstreamHandshakeTimeoutReportsFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			io.Copy(io.Discard, conn)
		}
	}()
	selector := &testSelector{}
	selector.set(listener.Addr().String(), "", "")
	var reports atomic.Int32
	gateway, _, _ := startGateway(t, &Server{Selector: selector, HandshakeTimeout: 100 * time.Millisecond, OnUpstreamError: func(routing.Ref, error) { reports.Add(1) }})
	client, status := connectClient(t, gateway, 1)
	client.Close()
	if status != 1 || reports.Load() != 1 {
		t.Fatalf("timeout result: status %d, reports %d", status, reports.Load())
	}
}

func TestTargetFailureReplyDoesNotReportProxyFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(conn, greeting); err != nil {
			return
		}
		conn.Write([]byte{5, 0})
		if _, _, err := readRequest(conn); err != nil {
			return
		}
		writeFailure(conn, 5)
	}()
	selector := &testSelector{}
	selector.set(listener.Addr().String(), "", "")
	var reports atomic.Int32
	gateway, _, _ := startGateway(t, &Server{Selector: selector, OnUpstreamError: func(routing.Ref, error) { reports.Add(1) }})
	client, status := connectClient(t, gateway, 1)
	client.Close()
	if status != 5 || reports.Load() != 0 {
		t.Fatalf("target rejection: status %d, proxy failure reports %d", status, reports.Load())
	}
}

func TestIdleConnectionIsClosed(t *testing.T) {
	target := startTarget(t, false)
	var accepts atomic.Int32
	upstream := startMockProxy(t, target, "", "", &accepts)
	selector := &testSelector{}
	selector.set(upstream, "", "")
	gateway, _, _ := startGateway(t, &Server{Selector: selector, IdleTimeout: 100 * time.Millisecond})
	client, status := connectClient(t, gateway, 1)
	defer client.Close()
	if status != 0 {
		t.Fatalf("CONNECT status %d", status)
	}
	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("client deadline expired before idle connection was closed")
	}
}
