package session

import (
	"testing"

	"golang.org/x/crypto/ssh"
)

// The PTY rules below were migrated from the legacy implementation
// (knot/pkg/daemon/handler_ssh_test.go) so the negotiated terminal parameters
// stay identical. The request-level behaviour, that these values actually reach
// the remote, is covered by TestSSHSessionRecordsPTYAndEnvironmentRequests.

func TestSSHTerminalModesKeepRemoteEchoEnabled(t *testing.T) {
	modes := sshTerminalModes()

	if got := modes[ssh.ECHO]; got != 1 {
		t.Fatalf("ssh ECHO mode = %d, want 1", got)
	}
}

func TestSSHTerminalModesUseStandardPTYSpeed(t *testing.T) {
	tests := []struct {
		name string
		mode uint8
		want uint32
	}{
		{name: "input speed", mode: ssh.TTY_OP_ISPEED, want: 38400},
		{name: "output speed", mode: ssh.TTY_OP_OSPEED, want: 38400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshTerminalModes()[tt.mode]; got != tt.want {
				t.Fatalf("mode %d = %d, want %d", tt.mode, got, tt.want)
			}
		})
	}
}

func TestValidTerminalDimensions(t *testing.T) {
	tests := []struct {
		name       string
		rows, cols int
		want       bool
	}{
		{name: "normal", rows: 24, cols: 80, want: true},
		{name: "minimum", rows: 1, cols: 1, want: true},
		{name: "maximum", rows: 1000, cols: 1000, want: true},
		{name: "zero rows", rows: 0, cols: 80},
		{name: "zero cols", rows: 24, cols: 0},
		{name: "negative", rows: -1, cols: 80},
		{name: "negative cols", rows: 24, cols: -1},
		{name: "too large rows", rows: 1001, cols: 80},
		{name: "too large cols", rows: 24, cols: 1001},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validTerminalDimensions(tt.rows, tt.cols); got != tt.want {
				t.Fatalf("validTerminalDimensions(%d, %d) = %v, want %v", tt.rows, tt.cols, got, tt.want)
			}
		})
	}
}

func TestValidSSHEnvName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "LANG", want: true},
		{name: "LC_CTYPE", want: true},
		{name: "COLORTERM", want: true},
		{name: "TERM_PROGRAM2", want: true},
		{name: "LC-CTYPE"},
		{name: "1LANG"},
		{name: "lang"},
		{name: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validSSHEnvName(tt.name); got != tt.want {
				t.Fatalf("validSSHEnvName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestValidSSHEnvValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "locale", value: "en_US.UTF-8", want: true},
		{name: "empty", value: "", want: true},
		{name: "path", value: "/usr/local/bin:/usr/bin", want: true},
		{name: "newline", value: "bad\nvalue"},
		{name: "carriage return", value: "bad\rvalue"},
		{name: "tilde", value: "bad\x7fvalue"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validSSHEnvValue(tt.value); got != tt.want {
				t.Fatalf("validSSHEnvValue(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
