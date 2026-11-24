// Package i18n provides compiled bilingual messages and current-user language detection.
package i18n

import (
	"bytes"
	"fmt"
	"text/template"
)

type Key string

const (
	Usage                  Key = "cli.usage"
	UsageCommand           Key = "cli.usage.command"
	UsageList              Key = "cli.usage.ls"
	UsageExec              Key = "cli.usage.exec"
	InvalidArguments       Key = "cli.invalid_arguments"
	TTYRequired            Key = "cli.tty_required"
	FrontendPending        Key = "cli.frontend_pending"
	StartupDirectoryFailed Key = "cli.startup_directory_failed"
	OutputFailed           Key = "cli.output_failed"
	MenuServers            Key = "menu.servers"
	MenuKeys               Key = "menu.keys"
	MenuProxies            Key = "menu.proxies"
	MenuSync               Key = "menu.sync"
	UnknownError           Key = "error.unknown"
)

var catalogs = map[Language]map[Key]string{
	English: {
		Usage: `Usage:
  knot [command]
  knot <alias>

Commands:
  server, servers   Open the servers page
  key, keys         Open the keys page
  proxy, proxies    Open the proxies page
  sync              Open the sync page
  ssh               Connect to a server
  ls                List servers
  exec              Run a remote command
  exit              Shut down knot-core
  help              Show help for a command

Flags:
  -h, --help        Show help
      --version     Show version

Use "knot <command> --help" for command help.
`,
		UsageCommand: `Usage:
  knot {{.Syntax}}

Flags:
  -h, --help  Show help
`,
		UsageList: `Usage:
  knot ls [--json] [pattern]

Flags:
  -h, --help  Show help
      --json  Output JSON
`,
		UsageExec: `Usage:
  knot exec [flags] <alias> "<command>"

Flags:
  -h, --help              Show help
      --json              Output JSON
      --session-key key   Reuse a remote shell by key
      --timeout duration  Set a time limit (e.g. 30s)
`,
		InvalidArguments:              "Invalid arguments: {{.Argument}}. Use knot --help.",
		"cli.core_shutdown_requested": "Core shutdown requested.",
		"cli.core_already_stopped":    "Core is not running.",
		TTYRequired:                   "This entry requires terminal input and output. Run knot in an interactive terminal.",
		FrontendPending:               "Management pages and SSH connections are still under development; this build supports knot --help and knot --version.",
		StartupDirectoryFailed:        "Cannot read the startup directory: {{.Reason}}.",
		OutputFailed:                  "Cannot write terminal output: {{.Reason}}.",
		MenuServers:                   "Servers", MenuKeys: "Keys", MenuProxies: "Proxies", MenuSync: "Sync",
		UnknownError: "Operation failed ({{.Code}}).",
	},
	Chinese: {
		Usage: `用法：
  knot [command]
  knot <alias>

命令：
  server, servers   打开服务器页
  key, keys         打开密钥页
  proxy, proxies    打开代理页
  sync              打开同步页
  ssh               连接服务器
  ls                列出服务器
  exec              执行远程命令
  exit              关闭 knot-core
  help              查看子命令帮助

选项：
  -h, --help        显示帮助
      --version     显示版本

使用 "knot <command> --help" 查看子命令帮助。
`,
		UsageCommand: `用法：
  knot {{.Syntax}}

选项：
  -h, --help  显示帮助
`,
		UsageList: `用法：
  knot ls [--json] [pattern]

选项：
  -h, --help  显示帮助
      --json  输出 JSON
`,
		UsageExec: `用法：
  knot exec [flags] <alias> "<command>"

选项：
  -h, --help              显示帮助
      --json              输出 JSON
      --session-key key   按键复用远程 shell
      --timeout duration  设置超时（如 30s）
`,
		InvalidArguments:              "参数无效：{{.Argument}}。请使用 knot --help。",
		"cli.core_shutdown_requested": "已请求 core 退出。",
		"cli.core_already_stopped":    "core 未运行。",
		TTYRequired:                   "此入口需要终端输入和输出，请在交互式终端中运行 knot。",
		FrontendPending:               "管理页面和 SSH 连接仍在开发中；当前构建支持 knot --help 和 knot --version。",
		StartupDirectoryFailed:        "无法读取启动目录：{{.Reason}}。",
		OutputFailed:                  "无法写入终端输出：{{.Reason}}。",
		MenuServers:                   "服务器", MenuKeys: "密钥", MenuProxies: "代理", MenuSync: "同步",
		UnknownError: "操作失败（{{.Code}}）。",
	},
}

var compiled = func() map[Language]map[Key]*template.Template {
	addUICatalogs()
	addManagementCatalogs()
	addSSHCatalogs()
	result := make(map[Language]map[Key]*template.Template, len(catalogs))
	for language, catalog := range catalogs {
		result[language] = make(map[Key]*template.Template, len(catalog))
		for key, value := range catalog {
			result[language][key] = template.Must(template.New(string(key)).Option("missingkey=error").Parse(value))
		}
	}
	return result
}()

type Translator struct{ language Language }

func (t Translator) Language() Language { return t.language }

func New(language Language) Translator {
	if language != Chinese {
		language = English
	}
	return Translator{language: language}
}

// Render reports unknown keys and missing parameters so callers cannot silently
// ship an incomplete translation. Values such as aliases are passed untouched.
func (t Translator) Render(key Key, params map[string]any) (string, error) {
	message, ok := compiled[t.language][key]
	if !ok {
		return "", fmt.Errorf("unknown message key %q", key)
	}
	var b bytes.Buffer
	if err := message.Execute(&b, params); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Text is for application messages. A catalog programming error falls back to
// a localized generic message retaining the stable key as a diagnostic code.
func (t Translator) Text(key Key, params map[string]any) string {
	message, err := t.Render(key, params)
	if err == nil {
		return message
	}
	message, _ = t.Render(UnknownError, map[string]any{"Code": string(key)})
	return message
}
