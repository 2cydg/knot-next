package proxyproto

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestHTTPInterimLongHeadersAndBufferedTunnel(t *testing.T) {
	for _, response := range []string{
		"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 Connection Established\r\n\r\npayload",
		"HTTP/1.1 201 Connected\r\nX-Long: " + strings.Repeat("a", 8192) + "\r\n\r\npayload",
	} {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer server.Close()
			r := bufio.NewReader(server)
			for {
				line, err := r.ReadString('\n')
				if err != nil || line == "\r\n" {
					break
				}
			}
			_, _ = io.WriteString(server, response)
		}()
		conn, err := Negotiate(context.Background(), client, Options{Type: "http"}, "example.invalid:443", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(conn)
		_ = conn.Close()
		<-done
		if err != nil || string(got) != "payload" {
			t.Fatal(string(got), err)
		}
	}
}
func TestHTTPHeaderBoundAndMalformedStatus(t *testing.T) {
	for _, tc := range []struct{ response, code string }{
		{"HTTP/1.1 200 Connected\r\nX: " + strings.Repeat("a", 65536) + "\r\n\r\n", "PROXY_RESPONSE_TOO_LARGE"},
		{"HTTP/1.1 nonsense\r\n\r\n", "PROXY_NEGOTIATION_FAILED"},
	} {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer server.Close()
			r := bufio.NewReader(server)
			for {
				line, err := r.ReadString('\n')
				if err != nil || line == "\r\n" {
					break
				}
			}
			_, _ = io.WriteString(server, tc.response)
		}()
		_, err := Negotiate(context.Background(), client, Options{Type: "http"}, "example.invalid:443", time.Second)
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != tc.code {
			t.Fatal(err, tc.code)
		}
		_ = client.Close()
		<-done
	}
}
func TestSOCKSNoMethodIsNegotiationFailure(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		var greeting [3]byte
		_, _ = io.ReadFull(server, greeting[:])
		_, _ = server.Write([]byte{5, 255})
	}()
	_, err := Negotiate(context.Background(), client, Options{Type: "socks5"}, "", time.Second)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "PROXY_NEGOTIATION_FAILED" {
		t.Fatal(err)
	}
	<-done
}
func TestTargetLengthValidation(t *testing.T) {
	if ValidateTarget(strings.Repeat("a", 300)+":443") == nil {
		t.Fatal("overlong target accepted")
	}
}
