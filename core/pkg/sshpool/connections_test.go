package sshpool

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestConnectionIdentityLatestAndPreciseLease(t *testing.T) {
	p, server, cfg := leaseFixture(t)
	n := 0
	p.idGenerator = func() (string, error) {
		n++
		if n < 3 {
			return "same", nil
		}
		if n == 3 {
			return "other", nil
		}
		return "new", nil
	}
	first, err := p.AcquireClientContext(context.Background(), server, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	reused, err := p.AcquireClientContext(context.Background(), server, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Release()
	if reused.ConnectionID != first.ConnectionID {
		t.Fatal("reuse changed identity")
	}
	rotated := server
	rotated.Password = "another-artificial-password"
	second, err := p.AcquireClientContext(context.Background(), rotated, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if first.ConnectionID == second.ConnectionID || n != 3 {
		t.Fatalf("ID collision not retried: %s %s %d", first.ConnectionID, second.ConnectionID, n)
	}
	again, err := p.AcquireClientContext(context.Background(), server, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Release()
	latest, err := p.AcquireConnection("", server.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Release()
	if latest.ConnectionID != second.ConnectionID {
		t.Fatal("reuse changed latest ordering")
	}
	if _, err := p.AcquireConnection(first.ConnectionID, "wrong-server"); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatal("cross-server transport acquired")
	}
	p.Clear()
	if _, err := p.AcquireConnection(first.ConnectionID, server.ID); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatal("detached transport acquired")
	}
	first.Release()
	second.Release()
	latest.Release()
	replacement, err := p.AcquireClientContext(context.Background(), server, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Release()
	if replacement.ConnectionID == first.ConnectionID || replacement.ConnectionID == second.ConnectionID {
		t.Fatal("retired identity rebound to new transport")
	}
}

func TestRetiredIdentitiesBoundedAndLiveIdentitiesKept(t *testing.T) {
	p, server, cfg := leaseFixture(t)
	lease, err := p.AcquireClientContext(context.Background(), server, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	p.mu.Lock()
	for i := 0; i < 5000; i++ {
		p.reservedIDs[fmt.Sprintf("retired_%d", i)] = time.Now().Add(-time.Duration(i) * time.Second)
	}
	p.reservedIDs[lease.ConnectionID] = time.Now().Add(-time.Hour)
	p.pruneIDsLocked()
	_, live := p.reservedIDs[lease.ConnectionID]
	count := len(p.reservedIDs)
	p.mu.Unlock()
	if !live || count > 4097 {
		t.Fatal("retention dropped live identity or exceeded retired capacity", live, count)
	}
	p.Clear()
	p.mu.Lock()
	count = len(p.reservedIDs)
	p.mu.Unlock()
	if count > 4096 {
		t.Fatal("clear exceeded retired capacity", count)
	}
}
