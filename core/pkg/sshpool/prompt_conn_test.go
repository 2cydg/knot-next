package sshpool

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestPromptObserverPreservesBytesAndDetectsDisconnect(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &promptConn{Conn: a}
	ended, stop := c.observe()
	payload := []byte{0, 255, 13, 10, 27, 0xe4, 0xb8, 0xad}
	go func() { _, _ = b.Write(payload); _ = b.Close() }()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("disconnect not observed")
	}
	stop()
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("observer changed handshake bytes", got, err)
	}
}

type countedPromptConn struct {
	net.Conn
	reads atomic.Int32
}

func (c *countedPromptConn) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.Conn.Read(p)
}

func TestPromptObserverAtCapacityStopsDetectingPeerClose(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	transport := &countedPromptConn{Conn: a}
	c := &promptConn{Conn: transport}
	ended, stop := c.observe()
	defer stop()
	payload := bytes.Repeat([]byte{0, 255, 13, 10}, (64<<10)/4)
	written := make(chan error, 1)
	go func() { _, err := b.Write(payload); written <- err }()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("observer did not fill its bounded buffer")
	}
	// Close while the confirmation still has no answer and before stop joins.
	_ = b.Close()
	stop()
	if len(c.buffered) != 64<<10 || transport.reads.Load() != 1 {
		t.Fatal("observer read beyond capacity", len(c.buffered), transport.reads.Load())
	}
	select {
	case <-ended:
		t.Fatal("capacity exhaustion was falsely reported as disconnect")
	default:
	}
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("full buffer was not replayed before EOF", len(got), err)
	}
}

func TestPromptObserverAtCapacityLeavesSubsequentBytesForHandshake(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	transport := &countedPromptConn{Conn: a}
	c := &promptConn{Conn: transport}
	ended, stop := c.observe()
	defer stop()
	payload := bytes.Repeat([]byte{255}, 64<<10)
	written := make(chan error, 1)
	tailDone := make(chan error, 1)
	tail := []byte("following-handshake-bytes")
	go func() {
		_, err := b.Write(payload)
		written <- err
		if err == nil {
			_, err = b.Write(tail)
		}
		tailDone <- err
		_ = b.Close()
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("observer never reached capacity")
	}
	stop()
	if len(c.buffered) != 64<<10 || transport.reads.Load() != 1 {
		t.Fatal("observer consumed bytes beyond capacity")
	}
	select {
	case <-ended:
		t.Fatal("full observation was falsely reported as disconnect")
	default:
	}
	_ = a.SetReadDeadline(time.Now().Add(time.Second))
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, append(payload, tail...)) {
		t.Fatal("bounded observation discarded or duplicated bytes", len(got), err)
	}
	if err := <-tailDone; err != nil {
		t.Fatal(err)
	}
}
func TestPromptObserverStopDoesNotInventDisconnect(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &promptConn{Conn: a}
	ended, stop := c.observe()
	stop()
	select {
	case <-ended:
		t.Fatal("interrupt read was reported as peer disconnect")
	default:
	}
	go func() { _, _ = b.Write([]byte("next")) }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "next" {
		t.Fatal("handshake could not resume", err)
	}
}
