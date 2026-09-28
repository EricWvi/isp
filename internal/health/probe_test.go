package health

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"isp/internal/config"
)

func startProbeProxy(t *testing.T, destination, username, password string) (config.Proxy, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	domains := make(chan string, 2)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(2 * time.Second))
				greeting := make([]byte, 3)
				if _, err := io.ReadFull(conn, greeting); err != nil || greeting[0] != 5 || greeting[1] != 1 {
					return
				}
				method := byte(0)
				if username != "" {
					method = 2
				}
				if greeting[2] != method {
					return
				}
				conn.Write([]byte{5, method})
				if method == 2 {
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
				request := make([]byte, 5)
				if _, err := io.ReadFull(conn, request); err != nil || request[0] != 5 || request[1] != 1 || request[3] != 3 {
					return
				}
				host := make([]byte, int(request[4]))
				if _, err := io.ReadFull(conn, host); err != nil {
					return
				}
				port := make([]byte, 2)
				if _, err := io.ReadFull(conn, port); err != nil {
					return
				}
				domains <- string(host)
				target, err := net.Dial("tcp", destination)
				if err != nil {
					return
				}
				defer target.Close()
				conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				conn.SetDeadline(time.Time{})
				done := make(chan struct{})
				go func() {
					io.Copy(target, conn)
					target.(*net.TCPConn).CloseWrite()
					close(done)
				}()
				io.Copy(conn, target)
				conn.(*net.TCPConn).CloseWrite()
				<-done
			}()
		}
	}()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	return config.Proxy{Host: host, Port: port, Username: username, Password: password}, domains
}

func TestHTTPProbeUsesSOCKSAndAcceptsNon2xx(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer httpServer.Close()
	parsed, err := url.Parse(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(parsed.Host)
	probeURL := "http://health.invalid:" + port + "/check"
	for _, tc := range []struct{ name, user, pass string }{{"no-auth", "", ""}, {"user-password", "alice", "secret"}} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, domains := startProbeProxy(t, parsed.Host, tc.user, tc.pass)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := HTTPProbe(ctx, proxy, probeURL); err != nil {
				t.Fatal(err)
			}
			select {
			case host := <-domains:
				if host != "health.invalid" {
					t.Fatalf("target domain resolved locally: %q", host)
				}
			case <-time.After(time.Second):
				t.Fatal("SOCKS proxy did not receive target domain")
			}
			if tc.user != "" {
				proxy.Password = "wrong"
				if err := HTTPProbe(ctx, proxy, probeURL); err == nil {
					t.Fatal("incorrect proxy password passed")
				}
			}
		})
	}
}

func TestHTTPUpstreamProbeUsesHTTPProxyAndRejectsBadCredentials(t *testing.T) {
	targets := make(chan string, 2)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targets <- r.URL.Host
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
		if r.Header.Get("Proxy-Authorization") != want {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer proxyServer.Close()
	host, portText, _ := net.SplitHostPort(proxyServer.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	proxy := config.Proxy{Host: host, Port: port, Username: "alice", Password: "secret"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := HTTPUpstreamProbe(ctx, proxy, "http://health.invalid/check"); err != nil {
		t.Fatal(err)
	}
	if host := <-targets; host != "health.invalid" {
		t.Fatalf("HTTP upstream did not receive target host: %q", host)
	}
	proxy.Password = "wrong"
	if err := HTTPUpstreamProbe(ctx, proxy, "http://health.invalid/check"); err == nil {
		t.Fatal("HTTP upstream 407 was treated as healthy")
	}
}
