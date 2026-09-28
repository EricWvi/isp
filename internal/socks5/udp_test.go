package socks5

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func startUDPProxy(t *testing.T, accepts *atomic.Int32) string {
	return startUDPProxyWithAuth(t, accepts, "", "")
}

func startUDPProxyWithAuth(t *testing.T, accepts *atomic.Int32, username, password string) string {
	t.Helper()
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close(); relay.Close() })
	go func() {
		packet := make([]byte, 65535)
		for {
			n, client, err := relay.ReadFromUDP(packet)
			if err != nil {
				return
			}
			_, _ = relay.WriteToUDP(packet[:n], client)
		}
	}()
	go func() {
		for {
			conn, err := control.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				greeting := make([]byte, 3)
				method := byte(0)
				if username != "" {
					method = 2
				}
				if _, err := io.ReadFull(conn, greeting); err != nil || !bytes.Equal(greeting, []byte{5, 1, method}) {
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
						return
					}
					conn.Write([]byte{1, 0})
				}
				request, _, err := readRequest(conn)
				if err != nil || request[1] != 3 {
					return
				}
				port := relay.LocalAddr().(*net.UDPAddr).Port
				// A wildcard BND.ADDR is common; the gateway must use the
				// upstream control connection's peer IP for UDP delivery.
				conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, byte(port >> 8), byte(port)})
				conn.SetDeadline(time.Time{})
				io.Copy(io.Discard, conn)
			}()
		}
	}()
	return control.Addr().String()
}

func TestUDPAssociateThroughAuthenticatedUpstream(t *testing.T) {
	var accepts atomic.Int32
	upstream := startUDPProxyWithAuth(t, &accepts, "alice", "secret")
	selector := &testSelector{}
	selector.set(upstream, "alice", "secret")
	gateway, _, _ := startGateway(t, &Server{Selector: selector})
	control, relay := associateClient(t, gateway)
	defer control.Close()
	defer relay.Close()
	packet := []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 53, 1, 2, 3}
	if _, err := relay.Write(packet); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 64)
	relay.SetReadDeadline(time.Now().Add(time.Second))
	n, err := relay.Read(response)
	if err != nil || !bytes.Equal(response[:n], packet) || accepts.Load() != 1 {
		t.Fatalf("authenticated UDP relay: %x, %v, accepts=%d", response[:n], err, accepts.Load())
	}
}

func associateClient(t *testing.T, gateway string) (net.Conn, *net.UDPConn) {
	t.Helper()
	control, err := net.DialTimeout("tcp", gateway, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	control.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(control, method); err != nil || !bytes.Equal(method, []byte{5, 0}) {
		t.Fatalf("client greeting: %x, %v", method, err)
	}
	if _, err := control.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	reply, err := readReply(control)
	if err != nil || reply[1] != 0 || reply[3] != 1 {
		t.Fatalf("UDP ASSOCIATE reply: %x, %v", reply, err)
	}
	control.SetDeadline(time.Time{})
	address := &net.UDPAddr{IP: net.IP(reply[4:8]), Port: int(reply[8])<<8 | int(reply[9])}
	relay, err := net.DialUDP("udp", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	return control, relay
}

func TestUDPAssociateRelaysDomainDatagram(t *testing.T) {
	var accepts atomic.Int32
	upstream := startUDPProxy(t, &accepts)
	selector := &testSelector{}
	selector.set(upstream, "", "")
	gateway, _, _ := startGateway(t, &Server{Selector: selector})
	control, relay := associateClient(t, gateway)
	defer control.Close()
	defer relay.Close()
	packet := append([]byte{0, 0, 0, 3, 14}, []byte("example.invalid")...)
	packet = append(packet, 0, 53, 1, 2, 3)
	if _, err := relay.Write(packet); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 512)
	relay.SetReadDeadline(time.Now().Add(time.Second))
	n, err := relay.Read(response)
	if err != nil || !bytes.Equal(response[:n], packet) || accepts.Load() != 1 {
		t.Fatalf("UDP relay: %x, %v, accepts=%d", response[:n], err, accepts.Load())
	}
}

func TestUDPAssociateRejectsKnownUnsupportedProxyWithoutDialing(t *testing.T) {
	var accepts atomic.Int32
	upstream := startUDPProxy(t, &accepts)
	selector := &testSelector{}
	selector.set(upstream, "", "")
	selector.mu.Lock()
	selector.snapshot.Proxy.UDPCapability = "unsupported"
	selector.mu.Unlock()
	gateway, _, _ := startGateway(t, &Server{Selector: selector})
	control, status := connectWithRequest(t, gateway, []byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	defer control.Close()
	if status != 7 || accepts.Load() != 0 {
		t.Fatalf("known unsupported UDP proxy: status=%d, upstream accepts=%d", status, accepts.Load())
	}
}

func TestUDPAssociateRemembersUpstreamRejection(t *testing.T) {
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	var accepts atomic.Int32
	go func() {
		for {
			conn, err := control.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				greeting := make([]byte, 3)
				if _, err := io.ReadFull(conn, greeting); err != nil {
					return
				}
				conn.Write([]byte{5, 0})
				if _, _, err := readRequest(conn); err != nil {
					return
				}
				writeFailure(conn, 7)
			}()
		}
	}()
	selector := &testSelector{}
	selector.set(control.Addr().String(), "", "")
	gateway, _, _ := startGateway(t, &Server{Selector: selector})
	for range 2 {
		client, status := connectWithRequest(t, gateway, []byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
		client.Close()
		if status != 7 {
			t.Fatalf("expected command unsupported, got %d", status)
		}
	}
	if accepts.Load() != 1 {
		t.Fatalf("rejected UDP capability was not remembered: %d upstream connections", accepts.Load())
	}
}

func TestUDPAssociationKeepsItsOriginalUpstreamAfterSwitch(t *testing.T) {
	var firstAccepts, secondAccepts atomic.Int32
	first := startUDPProxy(t, &firstAccepts)
	second := startUDPProxy(t, &secondAccepts)
	selector := &testSelector{}
	selector.set(first, "", "")
	gateway, _, _ := startGateway(t, &Server{Selector: selector})
	firstControl, firstRelay := associateClient(t, gateway)
	defer firstControl.Close()
	defer firstRelay.Close()
	selector.set(second, "", "")
	secondControl, secondRelay := associateClient(t, gateway)
	defer secondControl.Close()
	defer secondRelay.Close()
	for _, relay := range []*net.UDPConn{firstRelay, secondRelay} {
		packet := []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 53, 9, 8, 7}
		if _, err := relay.Write(packet); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, 64)
		relay.SetReadDeadline(time.Now().Add(time.Second))
		n, err := relay.Read(response)
		if err != nil || !bytes.Equal(response[:n], packet) {
			t.Fatalf("UDP session after switch: %x, %v", response[:n], err)
		}
	}
	if firstAccepts.Load() != 1 || secondAccepts.Load() != 1 {
		t.Fatalf("sessions were not pinned: first=%d second=%d", firstAccepts.Load(), secondAccepts.Load())
	}
}

func TestUDPDNSAndQUICSizedDatagrams(t *testing.T) {
	var accepts atomic.Int32
	upstream := startUDPProxy(t, &accepts)
	selector := &testSelector{}
	selector.set(upstream, "", "")
	gateway, _, _ := startGateway(t, &Server{Selector: selector})
	control, relay := associateClient(t, gateway)
	defer control.Close()
	defer relay.Close()
	dnsQuery := []byte{0x12, 0x34, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	quicSized := bytes.Repeat([]byte{0xc0}, 1200)
	for _, tc := range []struct {
		name    string
		port    int
		payload []byte
	}{{"dns", 53, dnsQuery}, {"quic-sized", 443, quicSized}} {
		t.Run(tc.name, func(t *testing.T) {
			packet := append([]byte{0, 0, 0, 3, 14}, []byte("example.invalid")...)
			packet = append(packet, byte(tc.port>>8), byte(tc.port))
			packet = append(packet, tc.payload...)
			if _, err := relay.Write(packet); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, 2048)
			relay.SetReadDeadline(time.Now().Add(time.Second))
			n, err := relay.Read(response)
			if err != nil || !bytes.Equal(response[:n], packet) {
				t.Fatalf("UDP payload changed: %x, %v", response[:n], err)
			}
		})
	}
}

func TestUDPAssociationClosesOnIdleTimeout(t *testing.T) {
	var accepts atomic.Int32
	upstream := startUDPProxy(t, &accepts)
	selector := &testSelector{}
	selector.set(upstream, "", "")
	gateway, _, _ := startGateway(t, &Server{Selector: selector, IdleTimeout: 50 * time.Millisecond})
	control, relay := associateClient(t, gateway)
	defer control.Close()
	defer relay.Close()
	control.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := control.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("idle UDP association kept control connection open: n=%d, err=%v", n, err)
	}
}

func TestUDPAssociationDropsFragmentsAndClosesWithControl(t *testing.T) {
	var accepts atomic.Int32
	upstream := startUDPProxy(t, &accepts)
	selector := &testSelector{}
	selector.set(upstream, "", "")
	gateway, _, _ := startGateway(t, &Server{Selector: selector})
	control, relay := associateClient(t, gateway)
	defer relay.Close()
	fragment := []byte{0, 0, 1, 1, 127, 0, 0, 1, 0, 53, 1}
	if _, err := relay.Write(fragment); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 64)
	relay.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := relay.Read(response); err == nil {
		t.Fatal("fragmented UDP packet was forwarded")
	}
	packet := []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 53, 9}
	if _, err := relay.Write(packet); err != nil {
		t.Fatal(err)
	}
	relay.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := relay.Read(response); err != nil || !bytes.Equal(response[:n], packet) {
		t.Fatalf("valid packet was not forwarded: %x, %v", response[:n], err)
	}
	control.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _ = relay.Write(packet)
		relay.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		if _, err := relay.Read(response); err != nil {
			return
		}
	}
	t.Fatal("UDP relay remained active after control connection closed")
}
