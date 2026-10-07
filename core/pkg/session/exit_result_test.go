package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeExitStatus lets the exit-status extraction be tested without constructing
// an *ssh.ExitError, whose status field is not settable from outside the ssh
// package. The production path only requires ExitStatus() int, so a fake is a
// faithful stand-in, and wrapped errors are covered too.
type fakeExitStatus struct {
	status int
}

func (e *fakeExitStatus) Error() string { return fmt.Sprintf("exit status %d", e.status) }
func (e *fakeExitStatus) ExitStatus() int {
	return e.status
}

func TestComputeExitResult(t *testing.T) {
	zero, seven := 0, 7
	signal := -1

	tests := []struct {
		name        string
		err         error
		wantCode    *int
		wantErrText string
		wantCause   string
	}{
		{
			name:     "nil error is a clean exit",
			err:      nil,
			wantCode: &zero,
		},
		{
			name:     "eof is a clean exit",
			err:      io.EOF,
			wantCode: &zero,
		},
		{
			name:     "exit status zero stays a clean exit",
			err:      &fakeExitStatus{status: 0},
			wantCode: &zero,
		},
		{
			name:     "non-zero exit status is preserved",
			err:      &fakeExitStatus{status: seven},
			wantCode: &seven,
		},
		{
			name:      "negative status means the remote was signaled",
			err:       &fakeExitStatus{status: -1},
			wantCode:  &signal,
			wantCause: causeRemoteSignal,
		},
		{
			name:        "missing exit status is not success",
			err:         &ssh.ExitMissingError{},
			wantCause:   causeExitStatusMissing,
			wantErrText: "remote session ended without exit status",
		},
		{
			name:     "wrapped exit status is still found",
			err:      fmt.Errorf("session ended: %w", &fakeExitStatus{status: seven}),
			wantCode: &seven,
		},
		{
			name:      "canceled session is a client disconnect",
			err:       context.Canceled,
			wantCause: causeClientDisconnected,
		},
		{
			name:        "network error keeps its message",
			err:         errors.New("connection reset by peer"),
			wantErrText: "connection reset by peer",
			wantCause:   causeNetworkError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome := computeExitResult(tt.err)

			switch {
			case tt.wantCode == nil && outcome.Code != nil:
				t.Fatalf("code = %d, want nil", *outcome.Code)
			case tt.wantCode != nil && outcome.Code == nil:
				t.Fatalf("code = nil, want %d", *tt.wantCode)
			case tt.wantCode != nil && outcome.Code != nil && *outcome.Code != *tt.wantCode:
				t.Fatalf("code = %d, want %d", *outcome.Code, *tt.wantCode)
			}
			if outcome.FrameworkError != tt.wantErrText {
				t.Fatalf("framework error = %q, want %q", outcome.FrameworkError, tt.wantErrText)
			}
			if outcome.DisconnectCause != tt.wantCause {
				t.Fatalf("cause = %q, want %q", outcome.DisconnectCause, tt.wantCause)
			}

			// Only an explicit zero exit status may be reported as success: a
			// missing status, a disconnect and a network error never are.
			wantSuccess := tt.wantCode != nil && *tt.wantCode == 0 && tt.wantErrText == ""
			if outcome.Succeeded() != wantSuccess {
				t.Fatalf("Succeeded() = %v, want %v", outcome.Succeeded(), wantSuccess)
			}
			if wantSuccess && outcome.Err() != nil {
				t.Fatalf("Err() = %v, want nil", outcome.Err())
			}
			if !wantSuccess && outcome.Err() == nil {
				t.Fatal("Err() = nil, want an error")
			}
		})
	}
}
