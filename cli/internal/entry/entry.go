// Package entry parses launch intent without discovering core or touching a terminal.
package entry

import "fmt"

type Mode string
type Page string
type ReturnPolicy string

const (
	Management  Mode = "management"
	PageDirect  Mode = "page"
	AliasDirect Mode = "alias"
	Help        Mode = "help"
	Version     Mode = "version"
	List        Mode = "list"

	Servers  Page = "servers"
	Keys     Page = "keys"
	Proxies  Page = "proxies"
	Sync     Page = "sync"
	Settings Page = "settings"

	ReturnToTUI ReturnPolicy = "tui"
	ExitProgram ReturnPolicy = "exit"

	ExitOK       = 0
	ExitFailure  = 1
	ExitUsage    = 2
	ExitCanceled = 130
)

// LaunchContext is a value with read-only accessors. Alias is an unresolved core
// reference; later API resolution must use the returned persistent server ID.
type LaunchContext struct {
	mode   Mode
	page   Page
	alias  string
	policy ReturnPolicy
}

func (l LaunchContext) Mode() Mode                 { return l.mode }
func (l LaunchContext) InitialPage() Page          { return l.page }
func (l LaunchContext) ServerRef() string          { return l.alias }
func (l LaunchContext) ReturnPolicy() ReturnPolicy { return l.policy }

type UsageError struct {
	Argument string
}

func (e *UsageError) Error() string { return fmt.Sprintf("invalid arguments: %q", e.Argument) }

// commands is the single registry for top-level command dispatch and alias checks.
var commands = map[string]LaunchContext{
	"--help": {mode: Help, policy: ExitProgram}, "--version": {mode: Version, policy: ExitProgram},
	"server": pageLaunch(Servers), "servers": pageLaunch(Servers),
	"key": pageLaunch(Keys), "keys": pageLaunch(Keys),
	"proxy": pageLaunch(Proxies), "proxies": pageLaunch(Proxies),
	"sync": pageLaunch(Sync), "ssh": {mode: AliasDirect, policy: ExitProgram},
	"ls": {mode: List, policy: ExitProgram},
}

func IsCommand(name string) bool { _, ok := commands[name]; return ok }

func Parse(args []string) (LaunchContext, error) {
	if len(args) == 0 {
		return LaunchContext{mode: Management, page: Servers, policy: ReturnToTUI}, nil
	}
	if len(args) == 2 && args[0] == "ssh" && validAlias(args[1]) {
		return LaunchContext{mode: AliasDirect, alias: args[1], policy: ExitProgram}, nil
	}
	if len(args) == 2 && args[0] == "ls" && args[1] != "" {
		return LaunchContext{mode: List, alias: args[1], policy: ExitProgram}, nil
	}
	if len(args) != 1 {
		return LaunchContext{}, &UsageError{Argument: fmt.Sprint(args)}
	}
	if command, ok := commands[args[0]]; ok {
		if args[0] == "ssh" {
			return LaunchContext{}, &UsageError{Argument: args[0]}
		}
		return command, nil
	}
	if !validAlias(args[0]) {
		return LaunchContext{}, &UsageError{Argument: args[0]}
	}
	return LaunchContext{mode: AliasDirect, alias: args[0], policy: ExitProgram}, nil
}

func pageLaunch(page Page) LaunchContext {
	return LaunchContext{mode: PageDirect, page: page, policy: ReturnToTUI}
}

func validAlias(alias string) bool {
	return alias != "" && alias[0] != '-'
}

// RemoteExitCode preserves a reported SSH status. Missing or invalid status is
// a local failure, never a successful remote exit. Management controllers retain
// this result for display instead of exiting the application.
func RemoteExitCode(status *int) int {
	if status == nil || *status < 0 || *status > 255 {
		return ExitFailure
	}
	return *status
}
