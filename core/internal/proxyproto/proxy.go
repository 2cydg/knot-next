// Package proxyproto shares proxy negotiation between SSH dialing and probes.
package proxyproto

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

type Options struct{ Type, Username, Password string }
type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }
func fail(code string) error   { return &Error{Code: code} }

// Negotiate owns cancellation/deadlines for this socket until negotiation ends.
// An empty target performs SOCKS method/auth negotiation, or only HTTP TCP reachability.
func Negotiate(ctx context.Context, conn net.Conn, opts Options, target string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var result net.Conn = conn
	var err error
	switch opts.Type {
	case "socks5":
		err = socks(conn, opts, target)
	case "http":
		if target != "" {
			result, err = httpConnect(conn, opts, target)
		}
	default:
		err = fail("PROXY_NEGOTIATION_FAILED")
	}
	var classified *Error
	if ctx.Err() != nil && !errors.As(err, &classified) {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return result, nil
}
func socks(conn net.Conn, o Options, target string) error {
	if len(o.Username) > 255 || len(o.Password) > 255 {
		return fail("PROXY_AUTH_FAILED")
	}
	methods := []byte{5, 1, 0}
	if o.Username != "" {
		methods = []byte{5, 2, 0, 2}
	}
	if _, err := conn.Write(methods); err != nil {
		return err
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 5 {
		return fail("PROXY_NEGOTIATION_FAILED")
	}
	switch reply[1] {
	case 0:
	case 2:
		if o.Username == "" {
			return fail("PROXY_AUTH_FAILED")
		}
		msg := append([]byte{1, byte(len(o.Username))}, []byte(o.Username)...)
		msg = append(msg, byte(len(o.Password)))
		msg = append(msg, []byte(o.Password)...)
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply[:]); err != nil {
			return err
		}
		if reply[0] != 1 || reply[1] != 0 {
			return fail("PROXY_AUTH_FAILED")
		}
	default:
		return fail("PROXY_NEGOTIATION_FAILED")
	}
	if target == "" {
		return nil
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return fail("PROXY_TARGET_FAILED")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return fail("PROXY_TARGET_FAILED")
	}
	req := []byte{5, 1, 0}
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		req = append(req, 1)
		req = append(req, ip4...)
	} else if ip != nil {
		req = append(req, 4)
		req = append(req, ip.To16()...)
	} else {
		if len(host) == 0 || len(host) > 255 {
			return fail("PROXY_TARGET_FAILED")
		}
		req = append(req, 3, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return err
	}
	if head[0] != 5 || head[2] != 0 {
		return fail("PROXY_NEGOTIATION_FAILED")
	}
	if head[1] != 0 {
		return fail("PROXY_TARGET_FAILED")
	}
	n := 0
	switch head[3] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		var b [1]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return err
		}
		n = int(b[0])
	default:
		return fail("PROXY_NEGOTIATION_FAILED")
	}
	_, err = io.CopyN(io.Discard, conn, int64(n+2))
	return err
}
func httpConnect(conn net.Conn, o Options, target string) (net.Conn, error) {
	if strings.ContainsAny(target+o.Username+o.Password, "\r\n\x00") {
		return nil, fail("PROXY_NEGOTIATION_FAILED")
	}
	auth := ""
	if o.Username != "" {
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(o.Username+":"+o.Password)) + "\r\n"
	}
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n"+auth+"\r\n"); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	size := 0
	readLine := func() (string, error) {
		var line []byte
		for {
			part, err := reader.ReadSlice('\n')
			size += len(part)
			if size > 64<<10 {
				return "", fail("PROXY_RESPONSE_TOO_LARGE")
			}
			line = append(line, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			return string(line), err
		}
	}
	for {
		status, err := readLine()
		if err != nil {
			return nil, err
		}
		parts := strings.Fields(status)
		if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
			return nil, fail("PROXY_NEGOTIATION_FAILED")
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil || len(parts[1]) != 3 || code < 100 || code > 599 {
			return nil, fail("PROXY_NEGOTIATION_FAILED")
		}
		for {
			line, err := readLine()
			if err != nil {
				return nil, err
			}
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		if code >= 100 && code < 200 {
			continue
		}
		if code == 407 {
			return nil, fail("PROXY_AUTH_FAILED")
		}
		if code < 200 || code >= 300 {
			return nil, fail("PROXY_TARGET_FAILED")
		}
		break
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func ValidateTarget(target string) error {
	host, p, err := net.SplitHostPort(target)
	if err != nil || host == "" || len(host) > 255 || strings.ContainsAny(host, " /\t\r\n\x00") || (strings.Contains(host, ":") && net.ParseIP(host) == nil) {
		return fmt.Errorf("invalid target address")
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid target port")
	}
	return nil
}
