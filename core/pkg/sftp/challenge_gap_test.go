package sftp

import (
	"context"
	"errors"
	"knot-core/pkg/config"
	"knot-core/pkg/sshpool"
	"sync/atomic"
	"testing"
	"time"
)

func TestChallengeDeadlineIdentityAndAcceptedWinner(t *testing.T) {
	for _, acceptBefore := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "accepted"}[acceptBefore], func(t *testing.T) {
			s := NewService(t.TempDir())
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			s.challengeNow = func() time.Time { return time.Unix(0, clock.Load()) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.sessions["pending"] = &resource{Session: Session{ID: "pending", State: "connecting"}, cancel: cancel}
			result := make(chan bool, 1)
			go func() {
				result <- s.waitHostKeyResponse(ctx, "pending", config.ServerProfile{}, sshpool.HostKeyPrompt{})
			}()
			deadline := time.Now().Add(time.Second)
			var c Challenge
			for {
				c, _ = s.HostKeyChallenge("pending")
				if c.Pending {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no challenge")
				}
				time.Sleep(time.Millisecond)
			}
			if c.ID == "" || c.ExpiresAt.IsZero() {
				t.Fatal("challenge identity/deadline absent")
			}
			if _, err := s.RespondHostKeyChallenge("pending", ChallengeResponse{ChallengeID: "wrong", Accept: true}); !errors.Is(err, ErrConflict) {
				t.Fatal("wrong round accepted")
			}
			if acceptBefore {
				_, err := s.RespondHostKeyChallenge("pending", ChallengeResponse{ChallengeID: c.ID, Accept: true})
				if err != nil {
					t.Fatal(err)
				}
				cancel() // accepted answers must survive the simultaneous cancellation branch
				clock.Add(int64(time.Minute))
				if s.failPendingChallenge("pending", "host_key", "host key confirmation timed out") {
					t.Fatal("accepted answer expired")
				}
				if !<-result {
					t.Fatal("accepted answer was discarded")
				}
				if _, err = s.RespondHostKeyChallenge("pending", ChallengeResponse{ChallengeID: c.ID, Accept: true}); !errors.Is(err, ErrConflict) {
					t.Fatal("duplicate answered")
				}
			} else {
				clock.Add(int64(time.Minute))
				if _, err := s.RespondHostKeyChallenge("pending", ChallengeResponse{ChallengeID: c.ID, Accept: true}); !errors.Is(err, ErrHostKeyChallengeExpired) {
					t.Fatal(err)
				}
				if <-result {
					t.Fatal("expired answer accepted")
				}
				got, _ := s.Get("pending")
				if got.State != "failed" || got.LastChallenge == nil || got.LastChallenge.Outcome != "expired" || got.FailureCode != "HOST_KEY_CHALLENGE_EXPIRED" {
					t.Fatalf("expiry snapshot: %+v", got)
				}
				if _, err := s.RespondHostKeyChallenge("pending", ChallengeResponse{ChallengeID: c.ID, Accept: true}); !errors.Is(err, ErrHostKeyChallengeExpired) {
					t.Fatal("expiry reason lost")
				}
			}
		})
	}
}
func TestAuthDeadlineReturnsStableExpiredReason(t *testing.T) {
	s := NewService(t.TempDir())
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.challengeNow = func() time.Time { return time.Unix(0, clock.Load()) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.sessions["pending"] = &resource{Session: Session{ID: "pending", State: "connecting"}, cancel: cancel}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.waitAuthResponse(ctx, "pending", config.ServerProfile{}, config.RuntimeConfig{}, sshpool.ErrAuthFailed, 1)
	}()
	var c Challenge
	deadline := time.Now().Add(time.Second)
	for {
		c, _ = s.AuthChallenge("pending")
		if c.Pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no challenge")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Add(int64(time.Minute))
	if _, err := s.RespondAuthChallenge("pending", ChallengeResponse{ChallengeID: c.ID, Password: "late-secret", Remember: true}); !errors.Is(err, ErrAuthChallengeExpired) {
		t.Fatal(err)
	}
	<-done
	got, _ := s.Get("pending")
	if got.FailureCode != "AUTH_CHALLENGE_EXPIRED" || got.LastChallenge.Outcome != "expired" {
		t.Fatalf("auth expiry: %+v", got)
	}
}

func TestWallClockRollbackCannotAcceptMonotonicallyExpiredChallenge(t *testing.T) {
	s := NewService(t.TempDir())
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.challengeNow = func() time.Time { return time.Unix(0, clock.Load()) }
	var wall atomic.Int64
	wall.Store(clock.Load())
	s.policy.Now = func() time.Time { return time.Unix(0, wall.Load()) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.sessions["pending"] = &resource{Session: Session{ID: "pending", State: "connecting"}, cancel: cancel}
	result := make(chan bool, 1)
	go func() {
		result <- s.waitHostKeyResponse(ctx, "pending", config.ServerProfile{}, sshpool.HostKeyPrompt{})
	}()
	deadline := time.Now().Add(time.Second)
	var c Challenge
	for {
		c, _ = s.HostKeyChallenge("pending")
		if c.Pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no challenge")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Add(int64(time.Minute))
	wall.Add(-int64(time.Hour))
	if _, err := s.RespondHostKeyChallenge("pending", ChallengeResponse{ChallengeID: c.ID, Accept: true}); !errors.Is(err, ErrHostKeyChallengeExpired) {
		t.Fatal("wall clock allowed late trust", err)
	}
	if <-result {
		t.Fatal("expired response accepted")
	}
}
