package socks5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"isp/internal/config"
	"isp/internal/routing"
	"isp/internal/tunnel"
)

const (
	version        = 5
	connectCommand = 1
	udpCommand     = 3
	noAuth         = 0
	userPassword   = 2
)

type Selector interface {
	Snapshot() (routing.Snapshot, bool)
}

type Server struct {
	Selector         Selector
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	OnUpstreamError  func(routing.Ref, error)
	udpMu            sync.RWMutex
	udpObserved      map[routing.Ref]udpObservation
}

type udpObservation struct {
	proxy      config.Proxy
	capability string
}

// UDPCapability reports configured or recently observed upstream support.
// Observations expire automatically when the proxy configuration changes.
func (s *Server) UDPCapability(ref routing.Ref, proxy config.Proxy) string {
	if proxy.UDPCapability == "unsupported" {
		return "unsupported"
	}
	s.udpMu.RLock()
	observed, ok := s.udpObserved[ref]
	s.udpMu.RUnlock()
	if ok && observed.proxy == proxy {
		return observed.capability
	}
	if proxy.UDPCapability == "supported" {
		return "supported"
	}
	return "unknown"
}

func (s *Server) observeUDP(ref routing.Ref, proxy config.Proxy, capability string) {
	s.udpMu.Lock()
	if s.udpObserved == nil {
		s.udpObserved = make(map[routing.Ref]udpObservation)
	}
	s.udpObserved[ref] = udpObservation{proxy: proxy, capability: capability}
	s.udpMu.Unlock()
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if s.Selector == nil {
		return errors.New("SOCKS5 selector is required")
	}
	var mu sync.Mutex
	active := make(map[net.Conn]struct{})
	var handlers sync.WaitGroup
	stop := context.AfterFunc(ctx, func() {
		listener.Close()
		mu.Lock()
		for conn := range active {
			conn.Close()
		}
		mu.Unlock()
	})
	defer func() {
		listener.Close()
		mu.Lock()
		for conn := range active {
			conn.Close()
		}
		mu.Unlock()
		handlers.Wait()
		stop()
	}()
	var retryDelay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !temporaryAcceptError(err) {
				return err
			}
			// Back off like net/http so running out of descriptors pauses new
			// connections instead of stopping the service.
			retryDelay = min(max(2*retryDelay, 5*time.Millisecond), time.Second)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retryDelay):
			}
			continue
		}
		retryDelay = 0
		mu.Lock()
		active[conn] = struct{}{}
		handlers.Add(1)
		mu.Unlock()
		go func() {
			defer handlers.Done()
			defer conn.Close()
			defer func() {
				mu.Lock()
				delete(active, conn)
				mu.Unlock()
			}()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) handle(ctx context.Context, client net.Conn) {
	handshakeTimeout := durationOr(s.HandshakeTimeout, 15*time.Second)
	client.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := negotiateClient(client); err != nil {
		return
	}
	request, status, err := readRequest(client)
	if err != nil {
		if status != 0 {
			writeFailure(client, status)
		}
		return
	}
	client.SetDeadline(time.Time{})
	snapshot, ok := s.Selector.Snapshot()
	if !ok {
		client.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		writeFailure(client, 1)
		return
	}
	if request[1] == udpCommand && s.UDPCapability(snapshot.Ref, snapshot.Proxy) == "unsupported" {
		client.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		writeFailure(client, 7)
		return
	}
	address := net.JoinHostPort(snapshot.Proxy.Host, strconv.Itoa(snapshot.Proxy.Port))
	dialCtx, cancel := context.WithTimeout(ctx, durationOr(s.DialTimeout, 10*time.Second))
	defer cancel()
	upstream, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	if err != nil {
		s.report(ctx, snapshot.Ref, err)
		client.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		writeFailure(client, 1)
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(ctx, func() { upstream.Close() })
	defer stop()
	upstream.SetDeadline(time.Now().Add(handshakeTimeout))
	reply, err := negotiateUpstream(upstream, snapshot.Proxy.Username, snapshot.Proxy.Password, request)
	if err != nil {
		s.report(ctx, snapshot.Ref, err)
		client.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		writeFailure(client, 1)
		return
	}
	upstream.SetDeadline(time.Time{})
	if request[1] == udpCommand {
		if reply[1] != 0 {
			if reply[1] == 7 {
				s.observeUDP(snapshot.Ref, snapshot.Proxy, "unsupported")
			}
			client.SetWriteDeadline(time.Now().Add(handshakeTimeout))
			_ = writeAll(client, reply)
			return
		}
		s.observeUDP(snapshot.Ref, snapshot.Proxy, "supported")
		if err := s.serveUDPAssociation(ctx, client, upstream, reply, request); err != nil {
			client.SetWriteDeadline(time.Now().Add(handshakeTimeout))
			writeFailure(client, 1)
		}
		return
	}
	client.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	if err := writeAll(client, reply); err != nil || reply[1] != 0 {
		return
	}
	client.SetDeadline(time.Time{})
	tunnel.Relay(ctx, client, client, upstream, durationOr(s.IdleTimeout, 5*time.Minute))
}

func (s *Server) report(ctx context.Context, ref routing.Ref, err error) {
	if ctx.Err() == nil && s.OnUpstreamError != nil {
		s.OnUpstreamError(ref, err)
	}
}

func negotiateClient(conn net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	if header[0] != version || header[1] == 0 {
		return errors.New("invalid SOCKS5 greeting")
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	for _, method := range methods {
		if method == noAuth {
			return writeAll(conn, []byte{version, noAuth})
		}
	}
	writeAll(conn, []byte{version, 0xff})
	return errors.New("no supported client authentication method")
}

// readRequest returns the original CONNECT bytes so domain names remain with
// the upstream proxy and are never resolved by the local gateway.
func readRequest(conn net.Conn) ([]byte, byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, 0, err
	}
	if header[0] != version || header[2] != 0 {
		return nil, 1, errors.New("invalid SOCKS5 request header")
	}
	if header[1] != connectCommand && header[1] != udpCommand {
		return nil, 7, errors.New("only CONNECT and UDP ASSOCIATE are supported")
	}
	var remaining int
	switch header[3] {
	case 1:
		remaining = 4 + 2
	case 4:
		remaining = 16 + 2
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return nil, 0, err
		}
		if length[0] == 0 {
			return nil, 8, errors.New("empty target domain")
		}
		header = append(header, length[0])
		remaining = int(length[0]) + 2
	default:
		return nil, 8, errors.New("unsupported address type")
	}
	address := make([]byte, remaining)
	if _, err := io.ReadFull(conn, address); err != nil {
		return nil, 0, err
	}
	return append(header, address...), 0, nil
}

func negotiateUpstream(conn net.Conn, username, password string, request []byte) ([]byte, error) {
	method := byte(noAuth)
	if username != "" {
		method = userPassword
	}
	if err := writeAll(conn, []byte{version, 1, method}); err != nil {
		return nil, err
	}
	choice := make([]byte, 2)
	if _, err := io.ReadFull(conn, choice); err != nil {
		return nil, err
	}
	if choice[0] != version || choice[1] != method {
		return nil, fmt.Errorf("upstream rejected authentication method: %x", choice)
	}
	if method == userPassword {
		if len(username) > 255 || len(password) > 255 {
			return nil, errors.New("upstream credentials exceed SOCKS5 limit")
		}
		auth := make([]byte, 0, len(username)+len(password)+3)
		auth = append(auth, 1, byte(len(username)))
		auth = append(auth, username...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, password...)
		if err := writeAll(conn, auth); err != nil {
			return nil, err
		}
		result := make([]byte, 2)
		if _, err := io.ReadFull(conn, result); err != nil {
			return nil, err
		}
		if result[0] != 1 || result[1] != 0 {
			return nil, errors.New("upstream authentication failed")
		}
	}
	if err := writeAll(conn, request); err != nil {
		return nil, err
	}
	return readReply(conn)
}

func readReply(conn net.Conn) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	if header[0] != version || header[2] != 0 {
		return nil, errors.New("invalid upstream SOCKS5 reply")
	}
	var remaining int
	switch header[3] {
	case 1:
		remaining = 4 + 2
	case 4:
		remaining = 16 + 2
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return nil, err
		}
		header = append(header, length[0])
		remaining = int(length[0]) + 2
	default:
		return nil, errors.New("invalid upstream address type")
	}
	address := make([]byte, remaining)
	if _, err := io.ReadFull(conn, address); err != nil {
		return nil, err
	}
	return append(header, address...), nil
}

func writeFailure(conn net.Conn, status byte) {
	_ = writeAll(conn, []byte{version, status, 0, 1, 0, 0, 0, 0, 0, 0})
}

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func temporaryAcceptError(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM)
}

func durationOr(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}
