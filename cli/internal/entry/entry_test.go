package entry

import (
	"errors"
	"testing"
)

func TestLaunchIntent(t *testing.T) {
	cases := []struct {
		args   []string
		mode   Mode
		page   Page
		alias  string
		policy ReturnPolicy
	}{
		{nil, Management, Servers, "", ReturnToTUI},
		{[]string{"server"}, PageDirect, Servers, "", ReturnToTUI},
		{[]string{"servers"}, PageDirect, Servers, "", ReturnToTUI},
		{[]string{"key"}, PageDirect, Keys, "", ReturnToTUI},
		{[]string{"keys"}, PageDirect, Keys, "", ReturnToTUI},
		{[]string{"proxy"}, PageDirect, Proxies, "", ReturnToTUI},
		{[]string{"proxies"}, PageDirect, Proxies, "", ReturnToTUI},
		{[]string{"sync"}, PageDirect, Sync, "", ReturnToTUI},
		{[]string{"生产-01"}, AliasDirect, "", "生产-01", ExitProgram},
		{[]string{"ssh", "key"}, AliasDirect, "", "key", ExitProgram},
		{[]string{"ssh", "--help"}, "", "", "", ""},
		{[]string{"--help"}, Help, "", "", ExitProgram},
		{[]string{"--version"}, Version, "", "", ExitProgram},
		// These are not registered pages; a real server with this alias may resolve.
		{[]string{"settings"}, AliasDirect, "", "settings", ExitProgram},
		{[]string{"status"}, AliasDirect, "", "status", ExitProgram},
		{[]string{"files"}, AliasDirect, "", "files", ExitProgram},
		{[]string{"KEY"}, AliasDirect, "", "KEY", ExitProgram},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode)+"/"+tc.alias+"/"+string(tc.page), func(t *testing.T) {
			launch, err := Parse(tc.args)
			if tc.mode == "" {
				if err == nil {
					t.Fatal("expected invalid arguments")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if launch.Mode() != tc.mode || launch.InitialPage() != tc.page || launch.ServerRef() != tc.alias || launch.ReturnPolicy() != tc.policy {
				t.Fatalf("wrong launch: %+v", launch)
			}
			if len(tc.args) > 0 {
				original := launch
				tc.args[0] = "changed"
				if launch != original {
					t.Fatal("launch changed with argv")
				}
			}
		})
	}
}

func TestRejectInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{""}, {"ssh"}, {"--json"}, {"-h"}, {"--help", "key"},
		{"ssh", ""}, {"ssh", "a", "b"}, {"key", "extra"}, {"alias", "extra"},
	} {
		launch, err := Parse(args)
		var usageErr *UsageError
		if !errors.As(err, &usageErr) || launch != (LaunchContext{}) {
			t.Fatalf("Parse(%q) = %+v, %v", args, launch, err)
		}
	}
}

func TestRemoteExitIsNeverInvented(t *testing.T) {
	if RemoteExitCode(nil) != ExitFailure {
		t.Fatal("missing exit status was successful")
	}
	for _, status := range []int{0, 7, 130, 255} {
		if got := RemoteExitCode(&status); got != status {
			t.Fatalf("exit %d became %d", status, got)
		}
	}
	for _, status := range []int{-1, 256} {
		if RemoteExitCode(&status) != ExitFailure {
			t.Fatalf("accepted status %d", status)
		}
	}
}
