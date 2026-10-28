package i18n

func addSSHCatalogs() {
	entries := map[Key][2]string{
		"ssh.connecting":                     {"Connecting to SSH… [Esc] Cancel", "正在连接 SSH… [Esc] 取消"},
		"ssh.cancel_hint":                    {"[Esc] Cancel", "[Esc] 取消"},
		"ssh.trust":                          {"Trust host key", "信任主机密钥"},
		"ssh.auth":                           {"SSH authentication", "SSH 认证"},
		"ssh.password":                       {"Password", "密码"},
		"ssh.passphrase":                     {"Private key passphrase (this attempt only)", "私钥口令（仅本次连接）"},
		"ssh.key":                            {"Private key", "私钥"},
		"ssh.remember":                       {"Remember successful credentials", "记住验证成功的凭据"},
		"ssh.canceled":                       {"SSH connection canceled.", "SSH 连接已取消。"},
		"ssh.ended":                          {"SSH session ended.", "SSH 会话已结束。"},
		"ssh.exit":                           {"SSH exited with status {{.Code}}.", "SSH 已退出，状态码 {{.Code}}。"},
		"ssh.detached":                       {"Terminal disconnected; SSH session {{.ID}} remains available through core.", "终端连接中断；SSH 会话 {{.ID}} 保留在 core 中。"},
		"ssh.truncated":                      {"Some terminal output was not retained or could not be delivered.", "部分终端输出未被保留或未能完整送达。"},
		"error.CONNECTION_FAILED":            {"SSH connection failed.", "SSH 连接失败。"},
		"error.AUTH_CHALLENGE_EXPIRED":       {"Authentication confirmation expired; reconnect.", "认证确认已超时，请重新连接。"},
		"error.CHALLENGE_ABORTED":            {"SSH confirmation was canceled or the connection closed.", "SSH 确认已取消或连接已关闭。"},
		"error.agent_forwarding_unavailable": {"SSH Agent forwarding is unavailable. Start an Agent with a loaded key, then reconnect.", "SSH Agent 转发不可用。请启动 Agent 并加载密钥后重新连接。"},
		"error.ATTACH_LOST":                  {"The SSH terminal stream disconnected.", "SSH 终端流已断开。"},
		"error.ATTACH_FAILED":                {"Cannot attach the SSH terminal.", "无法接入 SSH 终端。"},
		"error.CONTROL_FAILED":               {"The SSH terminal control request failed.", "SSH 终端控制请求失败。"},
		"error.TERMINAL_FAILED":              {"Cannot read, write or restore the native terminal.", "无法读取、写入或恢复本机终端。"},
		"error.SSH_EXIT_UNKNOWN":             {"SSH ended without a valid remote exit status.", "SSH 结束时未收到有效的远端退出状态。"},
		"error.SSH_STATE_UNKNOWN":            {"Core returned an unsupported SSH session state.", "core 返回了未支持的 SSH 会话状态。"},
		"error.TTY_REQUIRED":                 {"SSH requires native terminal input and output.", "SSH 需要本机终端输入和输出。"},
	}
	for key, pair := range entries {
		catalogs[English][key] = pair[0]
		catalogs[Chinese][key] = pair[1]
	}
}
