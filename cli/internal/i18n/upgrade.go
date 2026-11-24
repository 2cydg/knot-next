package i18n

func addUpgradeCatalogs() {
	messages := []struct {
		key     Key
		english string
		chinese string
	}{
		{"upgrade.available", "↑ {{.Tag}} · knot upgrade", "↑ {{.Tag}} · knot upgrade"},
		{"upgrade.current", "Knot CLI and Core are up to date.", "Knot CLI 和 Core 已是最新版本。"},
		{"upgrade.newer", "The installed components are at or ahead of the latest stable release ({{.Tag}}).", "已安装的组件均已达到或高于最新正式版（{{.Tag}}）。"},
		{"upgrade.dev", "This build does not support self-upgrade.", "此构建不支持自升级。"},
		{"upgrade.check_failed", "Could not check for updates ({{.Reason}}).", "无法检查更新（{{.Reason}}）。"},
		{"upgrade.installation", "Could not identify an upgradable CLI and Core pair ({{.Reason}}).", "无法识别可升级的 CLI 和 Core（{{.Reason}}）。"},
		{"upgrade.downgrade", "The latest release is older than an installed component. Upgrade canceled.", "最新正式版低于已安装的组件版本，已取消升级。"},
		{"upgrade.prompt", "Upgrade Knot CLI and Core to {{.Tag}}? [y/N] ", "将 Knot CLI 和 Core 升级到 {{.Tag}}？[y/N] "},
		{"upgrade.core_prompt", "Upgrade Knot CLI and Core to {{.Tag}}? This will disconnect SSH and SFTP sessions, stop port forwards, and interrupt ongoing transfers. [y/N] ", "将 Knot CLI 和 Core 升级到 {{.Tag}}？这会断开 SSH 和 SFTP 会话、停止端口转发，并中断进行中的传输。[y/N] "},
		{"upgrade.canceled", "Upgrade canceled.", "已取消升级。"},
		{"upgrade.confirm_failed", "Could not read upgrade confirmation ({{.Reason}}).", "无法读取升级确认（{{.Reason}}）。"},
		{"upgrade.confirm_terminal", "Run knot upgrade in an interactive terminal to confirm the update.", "请在交互式终端中运行 knot upgrade 并确认更新。"},
		{"upgrade.downloading", "Downloading Knot {{.Tag}}…", "正在下载 Knot {{.Tag}}…"},
		{"upgrade.download_failed", "Could not download or verify the update ({{.Reason}}).", "无法下载或校验更新（{{.Reason}}）。"},
		{"upgrade.core_inspect", "Could not verify the running Core ({{.Reason}}).", "无法确认正在运行的 Core（{{.Reason}}）。"},
		{"upgrade.core_other", "The configured Core runs from a different installation.", "当前配置连接的是另一套安装中的 Core。"},
		{"upgrade.core_changed", "The Core instance changed while preparing the upgrade. Run knot upgrade again.", "准备升级期间 Core 实例发生了变化，请重新运行 knot upgrade。"},
		{"upgrade.core_stopping", "Waiting for Core to stop…", "正在等待 Core 退出…"},
		{"upgrade.core_stop_failed", "Could not confirm that Core stopped ({{.Reason}}).", "无法确认 Core 已退出（{{.Reason}}）。"},
		{"upgrade.locked", "Could not acquire the installation lock ({{.Reason}}).", "无法获取安装锁（{{.Reason}}）。"},
		{"upgrade.install_failed", "Could not install the update ({{.Reason}}).", "无法安装更新（{{.Reason}}）。"},
		{"upgrade.restore_failed", "Could not restore the previous Core. The installation is locked for recovery. Backups are in {{.Path}}.", "无法恢复旧版 Core。安装已锁定以便恢复，备份位于 {{.Path}}。"},
		{"upgrade.handoff", "Update is ready. It will be applied when this command exits.", "更新已准备好，将在此命令退出后安装。"},
		{"upgrade.success", "Knot CLI and Core upgraded to {{.Tag}}. Previous versions are backed up in {{.Path}}.", "Knot CLI 和 Core 已升级到 {{.Tag}}。旧版本已备份到 {{.Path}}。"},
	}
	for _, message := range messages {
		catalogs[English][message.key] = message.english
		catalogs[Chinese][message.key] = message.chinese
	}
}
