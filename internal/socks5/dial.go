package socks5

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"isp/internal/config"
)

// DialContext opens a TCP target through the specified SOCKS5 upstream.
// Domain targets are sent to the proxy without local DNS resolution.
func DialContext(ctx context.Context, proxy config.Proxy, target string) (net.Conn, error) {
	request, err := connectRequest(target)
	if err != nil {
		return nil, err
	}
	upstream := net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", upstream)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(15 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	reply, err := negotiateUpstream(conn, proxy.Username, proxy.Password, request)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if reply[1] != 0 {
		conn.Close()
		return nil, fmt.Errorf("upstream CONNECT rejected target with status %d", reply[1])
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func connectRequest(target string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid target port")
	}
	request := []byte{version, connectCommand, 0}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			request = append(request, 1)
			request = append(request, v4...)
		} else {
			request = append(request, 4)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) == 0 || len(host) > 255 {
			return nil, errors.New("invalid target domain length")
		}
		request = append(request, 3, byte(len(host)))
		request = append(request, host...)
	}
	return append(request, byte(port>>8), byte(port)), nil
}
