package sshpool

import (
	"context"
	"errors"
	"knot-core/pkg/config"
	"testing"
)

func TestLegacyAcquisitionRetainsCompletePublishedRoute(t *testing.T) {
	p, jump, cfg := leaseFixture(t)
	jump.ID = "jump"
	middle := config.ServerProfile{ID: "middle", Host: "middle", Port: 22, JumpHostIDs: []string{jump.ID}}
	target := config.ServerProfile{ID: "target", Host: "target", Port: 22, JumpHostIDs: []string{jump.ID, middle.ID}}
	cfg.Servers = map[string]config.ServerProfile{jump.ID: jump, middle.ID: middle, target.ID: target}
	calls := 0
	p.UseTargetHook(func(_, serverID string, _ []config.ForwardDefinition) {
		if serverID != target.ID {
			t.Error(serverID)
		}
		calls++
	})
	_, _, _, err := p.GetClientContext(context.Background(), target, cfg, nil, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var targetID, middleID string
	for _, v := range p.Connections("") {
		switch v.ServerID {
		case target.ID:
			targetID = v.ID
			if len(v.Route) != 2 || !v.Target {
				t.Fatal(v)
			}
		case middle.ID:
			middleID = v.ID
			if len(v.Route) != 1 || v.Target {
				t.Fatal(v)
			}
		}
	}
	if calls != 1 {
		t.Fatal("legacy hook not invoked", calls)
	}
	lease, err := p.AcquireConnection(targetID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range p.Stats() {
		if v.RefCount != 1 {
			t.Fatal("full route missing references", v)
		}
	}
	lease.Release()
	if _, err = p.AcquireConnection("", middle.ID); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatal("intermediate selected as default", err)
	}
	exact, err := p.AcquireConnection(middleID, middle.ID)
	if err != nil {
		t.Fatal("explicit intermediate unavailable", err)
	}
	if len(exact.Keys) != 2 {
		t.Fatal("intermediate prefix missing", exact.Keys)
	}
	exact.Release()
	for _, v := range p.Stats() {
		if v.RefCount != 0 {
			t.Fatal("legacy references leaked", v)
		}
	}
}
