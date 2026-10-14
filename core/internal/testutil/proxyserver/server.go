// Package proxyserver supplies local SOCKS5/HTTP protocol fixtures.
package proxyserver

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

type Config struct {
	Type, Username, Password               string
	RejectNegotiation, RejectTarget, Stall bool
}
type Server struct {
	listener net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]bool
	workers  sync.WaitGroup
	cfg      Config
	closed   bool
}

func New(t *testing.T, cfg Config) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{listener: ln, conns: map[net.Conn]bool{}, cfg: cfg}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				_ = c.Close()
				return
			}
			s.conns[c] = true
			s.mu.Unlock()
			s.workers.Add(1)
			go s.handle(c)
		}
	}()
	t.Cleanup(func() {
		s.mu.Lock()
		s.closed = true
		_ = ln.Close()
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.mu.Unlock()
		s.workers.Wait()
	})
	return s
}
func (s *Server) Address() string { return s.listener.Addr().String() }
func (s *Server) handle(conn net.Conn) {
	defer s.workers.Done()
	defer conn.Close()
	defer func() { s.mu.Lock(); delete(s.conns, conn); s.mu.Unlock() }()
	if s.cfg.Stall {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	if s.cfg.RejectNegotiation {
		_, _ = conn.Write([]byte("invalid proxy protocol\r\n"))
		return
	}
	target := ""
	if s.cfg.Type == "http" {
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		if s.cfg.Username != "" && req.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(s.cfg.Username+":"+s.cfg.Password)) {
			_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
			return
		}
		target = req.Host
	} else {
		var head [2]byte
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			return
		}
		methods := make([]byte, int(head[1]))
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
		method := byte(0)
		if s.cfg.Username != "" {
			method = 2
		}
		_, _ = conn.Write([]byte{5, method})
		if method == 2 {
			if _, err := io.ReadFull(conn, head[:]); err != nil {
				return
			}
			user := make([]byte, int(head[1]))
			if _, err := io.ReadFull(conn, user); err != nil {
				return
			}
			var n [1]byte
			if _, err := io.ReadFull(conn, n[:]); err != nil {
				return
			}
			pass := make([]byte, int(n[0]))
			if _, err := io.ReadFull(conn, pass); err != nil {
				return
			}
			if string(user) != s.cfg.Username || string(pass) != s.cfg.Password {
				_, _ = conn.Write([]byte{1, 1})
				return
			}
			_, _ = conn.Write([]byte{1, 0})
		}
		var request [4]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil {
			return
		}
		var host string
		switch request[3] {
		case 1:
			b := make([]byte, 4)
			if _, err := io.ReadFull(conn, b); err != nil {
				return
			}
			host = net.IP(b).String()
		case 4:
			b := make([]byte, 16)
			if _, err := io.ReadFull(conn, b); err != nil {
				return
			}
			host = net.IP(b).String()
		case 3:
			var n [1]byte
			if _, err := io.ReadFull(conn, n[:]); err != nil {
				return
			}
			b := make([]byte, int(n[0]))
			if _, err := io.ReadFull(conn, b); err != nil {
				return
			}
			host = string(b)
		default:
			return
		}
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			return
		}
		target = net.JoinHostPort(host, strconv.Itoa(int(head[0])<<8|int(head[1])))
	}
	var remote net.Conn
	var err error
	if !s.cfg.RejectTarget {
		remote, err = net.Dial("tcp", target)
	}
	if s.cfg.RejectTarget || err != nil {
		if s.cfg.Type == "http" {
			_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		} else {
			_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		}
		return
	}
	defer remote.Close()
	s.mu.Lock()
	s.conns[remote] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.conns, remote); s.mu.Unlock() }()
	if s.cfg.Type == "http" {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	} else {
		_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(remote, conn); _ = remote.Close() }()
	_, _ = io.Copy(conn, remote)
	_ = conn.Close()
	<-done
}

func (s *Server) ConnectionCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.conns) }
