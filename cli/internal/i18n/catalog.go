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
  knot                       Open the server management TUI
  knot <alias>               Connect to SSH; exit when SSH ends
  knot server | servers      Open the servers page
  knot key | keys            Open the keys page
  knot proxy | proxies       Open the proxies page
  knot sync                  Open sync service configuration
  knot ssh <alias>           Connect to an alias matching a page keyword
  knot --help                Show this help without core or a TTY
  knot --version             Show the build version without core or a TTY

Command keywords stay in English. Page keywords take priority over aliases.
Start knot-core manually; the CLI never starts it or edits its configuration.
Exit codes: remote SSH status 0–255, local failure 1, usage error 2, cancel 130.
Missing SSH exit status is a failure. Management TUI stays open after SSH ends.

This build supports resource management, forwarding and native SSH through core.
SFTP file management and transfers are still under development.
`,
		InvalidArguments:       "Invalid arguments: {{.Argument}}. Use knot --help.",
		TTYRequired:            "This entry requires terminal input and output. Run knot in an interactive terminal.",
		FrontendPending:        "Management pages and SSH connections are still under development; this build supports knot --help and knot --version.",
		StartupDirectoryFailed: "Cannot read the startup directory: {{.Reason}}.",
		OutputFailed:           "Cannot write terminal output: {{.Reason}}.",
		MenuServers:            "Servers", MenuKeys: "Keys", MenuProxies: "Proxies", MenuSync: "Sync",
		UnknownError: "Operation failed ({{.Code}}).",
	},
	Chinese: {
		Usage: `用法：
  knot                       打开服务器管理 TUI
  knot <alias>               直连 SSH；SSH 结束后退出程序
  knot server | servers      打开服务器页
  knot key | keys            打开密钥页
  knot proxy | proxies       打开代理页
  knot sync                  打开同步服务配置页
  knot ssh <alias>           直连与页面关键字同名的别名
  knot --help                显示帮助，无需 core 或终端
  knot --version             显示构建版本，无需 core 或终端

命令关键字固定为英文，页面关键字优先于同名别名。
请手动启动 knot-core；前端不会启动服务或修改其配置文件。
退出码：远端 SSH 状态 0–255，本地失败 1，用法错误 2，主动取消 130。
缺少 SSH 退出状态视为失败；管理 TUI 在 SSH 结束后继续运行。

当前构建支持真实 core 资源管理、转发管理与原生 SSH 终端。
SFTP 文件管理及传输仍在开发中。
`,
		InvalidArguments:       "参数无效：{{.Argument}}。请使用 knot --help。",
		TTYRequired:            "此入口需要终端输入和输出，请在交互式终端中运行 knot。",
		FrontendPending:        "管理页面和 SSH 连接仍在开发中；当前构建支持 knot --help 和 knot --version。",
		StartupDirectoryFailed: "无法读取启动目录：{{.Reason}}。",
		OutputFailed:           "无法写入终端输出：{{.Reason}}。",
		MenuServers:            "服务器", MenuKeys: "密钥", MenuProxies: "代理", MenuSync: "同步",
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
