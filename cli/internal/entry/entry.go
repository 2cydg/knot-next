// Package entry parses launch intent without discovering core or touching a terminal.
package entry

import (
	"fmt"
	"strings"
	"time"
)

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
	Exec        Mode = "exec"
	StopCore    Mode = "stop-core"

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
	mode       Mode
	page       Page
	alias      string
	policy     ReturnPolicy
	command    string
	timeout    time.Duration
	json       bool
	sessionKey string
}

func (l LaunchContext) Mode() Mode                 { return l.mode }
func (l LaunchContext) InitialPage() Page          { return l.page }
func (l LaunchContext) ServerRef() string          { return l.alias }
func (l LaunchContext) ReturnPolicy() ReturnPolicy { return l.policy }
func (l LaunchContext) Command() string            { return l.command }
func (l LaunchContext) Timeout() time.Duration     { return l.timeout }
func (l LaunchContext) JSON() bool                 { return l.json }
func (l LaunchContext) SessionKey() string         { return l.sessionKey }

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
	"ls":   {mode: List, policy: ExitProgram},
	"exec": {mode: Exec, policy: ExitProgram},
	"exit": {mode: StopCore, policy: ExitProgram},
}

func IsCommand(name string) bool { _, ok := commands[name]; return ok }

func Parse(args []string) (LaunchContext, error) {
	if len(args) == 2 && (args[0] == "exec" || args[0] == "ls" || args[0] == "exit") && args[1] == "--help" {
		return LaunchContext{mode: Help, policy: ExitProgram}, nil
	}
	if len(args) > 0 && args[0] == "ls" {
		l := LaunchContext{mode: List, policy: ExitProgram}
		for _, arg := range args[1:] {
			if arg == "--json" && !l.json {
				l.json = true
				continue
			}
			if arg == "" || strings.HasPrefix(arg, "-") || l.alias != "" {
				return LaunchContext{}, &UsageError{Argument: fmt.Sprint(args)}
			}
			l.alias = arg
		}
		return l, nil
	}
	if len(args) > 0 && args[0] == "exec" {
		l := LaunchContext{mode: Exec, policy: ExitProgram}
		i := 1
		for i < len(args) && strings.HasPrefix(args[i], "--") {
			if args[i] == "--session-key" && i+1 < len(args) {
				l.sessionKey = args[i+1]
				if l.sessionKey == "" {
					return LaunchContext{}, &UsageError{Argument: fmt.Sprint(args)}
				}
				i += 2
				continue
			}
			if strings.HasPrefix(args[i], "--session-key=") {
				l.sessionKey = strings.TrimPrefix(args[i], "--session-key=")
				if l.sessionKey == "" {
					return LaunchContext{}, &UsageError{Argument: fmt.Sprint(args)}
				}
				i++
				continue
			}
			if args[i] == "--json" {
				l.json = true
				i++
				continue
			}
			v := ""
			if strings.HasPrefix(args[i], "--timeout=") {
				v = strings.TrimPrefix(args[i], "--timeout=")
				i++
			} else if args[i] == "--timeout" && i+1 < len(args) {
				v = args[i+1]
				i += 2
			} else {
				return LaunchContext{}, &UsageError{Argument: fmt.Sprint(args)}
			}
			d, err := time.ParseDuration(v)
			if err != nil || d < time.Millisecond {
				return LaunchContext{}, &UsageError{Argument: fmt.Sprint(args)}
			}
			l.timeout = d
		}
		if len(args)-i < 2 || !validAlias(args[i]) || strings.TrimSpace(strings.Join(args[i+1:], " ")) == "" {
			return LaunchContext{}, &UsageError{Argument: fmt.Sprint(args)}
		}
		l.alias, l.command = args[i], strings.Join(args[i+1:], " ")
		return l, nil
	}
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
