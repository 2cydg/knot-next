package main

import (
	"fmt"
	"os"
	"strings"
)

func printHelp(command string) error {
	_, err := fmt.Fprint(os.Stdout, coreHelp(command, chineseHelp(os.Getenv)))
	return err
}

func chineseHelp(getenv func(string) string) bool {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		locale := strings.TrimSpace(getenv(name))
		if locale == "" {
			continue
		}
		base, _, _ := strings.Cut(locale, ".")
		base, _, _ = strings.Cut(base, "@")
		if strings.EqualFold(base, "C") || strings.EqualFold(base, "POSIX") {
			return false
		}
		for _, preferred := range strings.Split(getenv("LANGUAGE"), ":") {
			if preferred = strings.TrimSpace(preferred); preferred != "" {
				return isChineseLocale(preferred)
			}
		}
		return isChineseLocale(locale)
	}
	return false
}

func isChineseLocale(locale string) bool {
	if i := strings.IndexAny(locale, "_-.@"); i >= 0 {
		locale = locale[:i]
	}
	return strings.EqualFold(locale, "zh")
}

func coreHelp(command string, chinese bool) string {
	if command != "" {
		syntax := command
		if command == "help" {
			syntax += " [command]"
		}
		if chinese {
			return "用法：\n  knot-core " + syntax + "\n\n选项：\n  -h, --help  显示帮助\n"
		}
		return "Usage:\n  knot-core " + syntax + "\n\nFlags:\n  -h, --help  Show help\n"
	}
	if chinese {
		return `用法：
  knot-core [--config <file>]
  knot-core <command>

命令：
  init             生成 config.toml
  help             查看子命令帮助

选项：
  -h, --help           显示帮助
      --config <file>  指定 Core 配置文件
`
	}
	return `Usage:
  knot-core [--config <file>]
  knot-core <command>

Commands:
  init             Create config.toml
  help             Show help for a command

Flags:
  -h, --help           Show help
      --config <file>  Use a core configuration file
`
}
