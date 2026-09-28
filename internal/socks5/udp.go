package socks5

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func (s *Server) serveUDPAssociation(ctx context.Context, client, upstream net.Conn, upstreamReply, request []byte) error {
	address, err := relayAddress(upstreamReply, upstream.RemoteAddr())
	if err != nil {
		return err
	}
	remote, err := net.DialUDP("udp", nil, address)
	if err != nil {
		return err
	}
	defer remote.Close()
	localIP := client.LocalAddr().(*net.TCPAddr).IP
	local, err := net.ListenUDP("udp", &net.UDPAddr{IP: localIP})
	if err != nil {
		return err
	}
	defer local.Close()
	reply := udpSuccessReply(local.LocalAddr().(*net.UDPAddr))
	client.SetWriteDeadline(time.Now().Add(durationOr(s.HandshakeTimeout, 15*time.Second)))
	if err := writeAll(client, reply); err != nil {
		return nil
	}
	client.SetDeadline(time.Time{})
	upstream.SetDeadline(time.Time{})
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	stop := context.AfterFunc(sessionCtx, func() {
		client.Close()
		upstream.Close()
		local.Close()
		remote.Close()
	})
	defer stop()
	watchControl := func(conn net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(io.Discard, conn)
		cancel()
	}
	wg.Add(2)
	go watchControl(client)
	go watchControl(upstream)
	clientIP := client.RemoteAddr().(*net.TCPAddr).IP
	requestedPort := udpRequestedPort(request)
	var peerMu sync.RWMutex
	var peer *net.UDPAddr
	var activity atomic.Int64
	activity.Store(time.Now().UnixNano())
	wg.Add(2)
	go func() {
		defer wg.Done()
		packet := make([]byte, 65535)
		for {
			n, source, err := local.ReadFromUDP(packet)
			if err != nil {
				cancel()
				return
			}
			if !source.IP.Equal(clientIP) || !validUDPDatagram(packet[:n]) || (requestedPort != 0 && source.Port != requestedPort) {
				continue
			}
			peerMu.Lock()
			if peer == nil {
				peer = source
			}
			allowed := peer.IP.Equal(source.IP) && peer.Port == source.Port
			peerMu.Unlock()
			if !allowed {
				continue
			}
			activity.Store(time.Now().UnixNano())
			if _, err := remote.Write(packet[:n]); err != nil {
				cancel()
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		packet := make([]byte, 65535)
		for {
			n, err := remote.Read(packet)
			if err != nil {
				cancel()
				return
			}
			if !validUDPDatagram(packet[:n]) {
				continue
			}
			peerMu.RLock()
			target := peer
			peerMu.RUnlock()
			if target == nil {
				continue
			}
			activity.Store(time.Now().UnixNano())
			if _, err := local.WriteToUDP(packet[:n], target); err != nil {
				cancel()
				return
			}
		}
	}()
	idle := durationOr(s.IdleTimeout, 5*time.Minute)
	interval := idle / 2
	if interval > time.Second {
		interval = time.Second
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-sessionCtx.Done():
			wg.Wait()
			return nil
		case <-ticker.C:
			if time.Since(time.Unix(0, activity.Load())) >= idle {
				cancel()
			}
		}
	}
}

func relayAddress(reply []byte, controlRemote net.Addr) (*net.UDPAddr, error) {
	if len(reply) < 6 || reply[0] != version || reply[1] != 0 || reply[2] != 0 {
		return nil, errors.New("invalid upstream UDP reply")
	}
	var host string
	var offset int
	switch reply[3] {
	case 1:
		if len(reply) != 10 {
			return nil, errors.New("invalid IPv4 UDP relay address")
		}
		host, offset = net.IP(reply[4:8]).String(), 8
	case 4:
		if len(reply) != 22 {
			return nil, errors.New("invalid IPv6 UDP relay address")
		}
		host, offset = net.IP(reply[4:20]).String(), 20
	case 3:
		if len(reply) < 7 || len(reply) != 7+int(reply[4]) || reply[4] == 0 {
			return nil, errors.New("invalid domain UDP relay address")
		}
		host, offset = string(reply[5:5+int(reply[4])]), 5+int(reply[4])
	default:
		return nil, errors.New("unsupported UDP relay address type")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = controlRemote.(*net.TCPAddr).IP.String()
	}
	port := int(reply[offset])<<8 | int(reply[offset+1])
	if port == 0 {
		return nil, errors.New("upstream UDP relay port is zero")
	}
	return net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
}

func udpSuccessReply(address *net.UDPAddr) []byte {
	if ip := address.IP.To4(); ip != nil {
		reply := []byte{version, 0, 0, 1}
		reply = append(reply, ip...)
		return append(reply, byte(address.Port>>8), byte(address.Port))
	}
	reply := []byte{version, 0, 0, 4}
	reply = append(reply, address.IP.To16()...)
	return append(reply, byte(address.Port>>8), byte(address.Port))
}

func udpRequestedPort(request []byte) int {
	return int(request[len(request)-2])<<8 | int(request[len(request)-1])
}

func validUDPDatagram(packet []byte) bool {
	if len(packet) < 4 || packet[0] != 0 || packet[1] != 0 || packet[2] != 0 {
		return false
	}
	switch packet[3] {
	case 1:
		return len(packet) >= 10
	case 4:
		return len(packet) >= 22
	case 3:
		return len(packet) >= 7 && packet[4] != 0 && len(packet) >= 7+int(packet[4])
	default:
		return false
	}
}
