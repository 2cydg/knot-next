package coreclient

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestConnectionRefusedIsUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	client, err := New("http://"+address, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = client.Version(ctx)
	if Code(err) != "UNREACHABLE" {
		t.Fatalf("connection refusal classified as %q: %v", Code(err), err)
	}
}
