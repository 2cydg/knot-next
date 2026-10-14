package sshserver

import (
	"context"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"strconv"
	"sync"
)

type forwardRequest struct {
	Address string
	Port    uint32
}

func (s *Server) handleGlobalRequests(conn *ssh.ServerConn, requests <-chan *ssh.Request) {
	listeners := map[string]net.Listener{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for request := range requests {
		var req forwardRequest
		switch request.Type {
		case "tcpip-forward":
			s.mu.Lock()
			started := s.forwardStarted
			reject := s.rejectForward
			s.mu.Unlock()
			select {
			case started <- struct{}{}:
			default:
			}
			if !s.waitBarrier(func(s *Server) chan struct{} { return s.beforeForward }) {
				return
			}
			if reject || ssh.Unmarshal(request.Payload, &req) != nil || req.Port > 65535 {
				_ = request.Reply(false, nil)
				continue
			}
			listener, err := net.Listen("tcp", net.JoinHostPort(req.Address, strconv.Itoa(int(req.Port))))
			if err != nil {
				_ = request.Reply(false, nil)
				continue
			}
			_, portText, _ := net.SplitHostPort(listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			req.Port = uint32(port)
			key := net.JoinHostPort(req.Address, portText)
			listeners[key] = listener
			_ = request.Reply(true, ssh.Marshal(struct{ Port uint32 }{uint32(port)}))
			s.goroutines.Add(1)
			go s.acceptForwarded(ctx, conn, listener, req)
		case "cancel-tcpip-forward":
			if ssh.Unmarshal(request.Payload, &req) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			key := net.JoinHostPort(req.Address, strconv.Itoa(int(req.Port)))
			listener := listeners[key]
			if listener == nil {
				_ = request.Reply(false, nil)
				continue
			}
			_ = listener.Close()
			delete(listeners, key)
			_ = request.Reply(true, nil)
		default:
			_ = request.Reply(false, nil)
		}
	}
}
func (s *Server) acceptForwarded(ctx context.Context, conn *ssh.ServerConn, listener net.Listener, req forwardRequest) {
	defer s.goroutines.Done()
	for {
		stream, err := listener.Accept()
		if err != nil {
			return
		}
		s.goroutines.Add(1)
		go func() {
			defer s.goroutines.Done()
			defer stream.Close()
			stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
			defer stop()
			origin, portText, _ := net.SplitHostPort(stream.RemoteAddr().String())
			port, _ := strconv.Atoi(portText)
			payload := ssh.Marshal(directTCPIPReq{DestAddr: req.Address, DestPort: req.Port, OriginAddr: origin, OriginPort: uint32(port)})
			channel, requests, err := conn.OpenChannel("forwarded-tcpip", payload)
			if err != nil {
				return
			}
			defer channel.Close()
			s.goroutines.Add(1)
			go func() { defer s.goroutines.Done(); ssh.DiscardRequests(requests) }()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _, _ = io.Copy(channel, stream); _ = channel.CloseWrite() }()
			go func() {
				defer wg.Done()
				_, _ = io.Copy(stream, channel)
				if tcp, ok := stream.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
			}()
			wg.Wait()
		}()
	}
}
