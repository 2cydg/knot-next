# AGENTS.md

## Project Philosophy

Knot Core is a local capability daemon for SSH, SFTP, forwarding, sync, archive, update, and related runtime management. It is designed to serve native terminals, browser-based clients, scripts, GUI frontends, and AI agents through one stable local API surface instead of a CLI-only private protocol.

The project favors a versioned `HTTP + JSON` management API, `WebSocket` for interactive or streaming flows, loopback-only exposure, token-based local authentication, platform-backed encryption for sensitive data, and structured responses that work well in scripts and automation.

## Core Logic

- `cmd/core` is the `knot-core` program entrypoint. It boots the local loopback-only service and should stay thin, delegating lifecycle wiring to internal and package-level services.
- `internal/api/http` owns the versioned REST-style management API. Model resources around stable domain objects and tasks rather than RPC-shaped command names.
- `internal/api/ws` owns interactive and streaming APIs over WebSocket. Use it for SSH attach, SFTP transfer progress, global events, log follow, and challenge/response flows, not for ordinary CRUD.
- `internal/api/response` is the shared response layer. Successful responses should remain structured and machine-friendly; failures should converge on one error envelope with fields like `code`, `message`, `details`, `retryable`, `risk`, `resource`, and `suggested_action`.
- `internal/auth` enforces the local security model: token authentication for both HTTP and WebSocket, optional browser `Origin` allowlisting as a secondary check, and rejection by default for unauthenticated requests.
- `internal/transport` owns listener and connection concerns. The daemon must only listen on `127.0.0.1` and `::1`; port is configurable, but non-loopback binding is not.
- `internal/runtime` and `internal/paths` own runtime discovery and user-scoped filesystem locations, including PID, port, token path, log path, startup time, and related state published for clients.
- `pkg/core` composes top-level capabilities and shared runtime services, but does not become a second transport layer or alternate public API.
- `pkg/config` owns configuration models, persistence, validation, metadata, migration checks, and migration planning. Config writes should go through core as the single authority.
- `pkg/secret` and `pkg/crypto` own secret state and encryption-provider behavior. Default APIs must expose secret presence and capability state without returning plaintext secrets.
- `pkg/session` owns SSH session resources, non-interactive exec, attach lifecycle, host key challenge flow, auth retry flow, and session state transitions.
- `pkg/sshpool` manages reusable SSH client state and pooling beneath session, SFTP, and forward operations.
- `pkg/sftp` owns SFTP session resources, remote file operations, upload/download flows, and progress event publication.
- `pkg/forward` owns local, remote, and dynamic forwarding resources, runtime state, and streaming events.
- `pkg/broadcast` owns SSH input broadcast groups and member lifecycle.
- `pkg/sync`, `pkg/archive`, `pkg/update`, and `pkg/task` should model long-running, previewable, observable operations as first-class task-driven resources rather than ad hoc command handlers.
- Preserve a hard separation between management semantics and raw SSH byte streams: HTTP/JSON handles resource control, while interactive PTY data must pass through WebSocket without interpretation, newline rewriting, ANSI parsing, or encoding assumptions.

## Reference Implementation

Business logic that already exists in validated external projects may be placed under `reference/` as local read-only reference material. The contents of `reference/` are intentionally ignored by Git, except for the directory placeholder.

- Before implementing equivalent behavior, read the matching reference code and its tests when they are available.
- Preserve validated behavior, edge cases, error semantics, and compatibility expectations unless the current `knot-core` design documents explicitly require a different behavior.
- Adapt reference behavior into the current daemon architecture and package boundaries; do not blindly copy old project structure, CLI-only flows, transport assumptions, or unrelated abstractions.
- Treat `reference/` as read-only unless the user explicitly asks to modify files there.
- Do not force-add, stage, or commit code from `reference/` unless the user explicitly asks.

## Collaboration Rules

- Do not run `git commit` until the user explicitly asks to commit code.
- When committing code, the commit message must follow Conventional Commits, for example `feat: add sftp batch command` or `fix: handle stale daemon socket`.
- Always respect `.gitignore`; do not force-add, stage, or otherwise bypass ignored paths unless the user explicitly asks.
- Keep Go code idiomatic, follow the existing package boundaries and style, and avoid unrelated refactors.
- For terminal-facing SSH features, preserve the byte stream from the remote server to the local terminal as faithfully as possible. Avoid filtering, buffering, rewriting, or interpreting PTY output unless the behavior is explicitly required and covered by focused regression tests.
- Avoid adding new dependencies unless they are necessary. Prefer the Go standard library first, then third-party packages only when the benefit is clear, because binary size matters for this project.
- When modifying older code that lacks test coverage, add focused unit tests or regression tests where practical.
- When adding tests, pay special attention to Windows compatibility: use `filepath` for local paths, avoid Unix-only assumptions, and consider Windows CI behavior for sockets, terminals, file locking, and platform-backed crypto.
- Do not let tests depend on live platform credential stores such as macOS Keychain, Windows DPAPI, or Linux Secret Service when asserting encrypted config behavior; inject a deterministic test crypto provider so CI can decrypt data across repeated loads.
- After core logic changes, prefer running `go test ./...`; for release or build-related changes, also confirm `go build -o knot-core cmd/core/main.go` succeeds.

## Workflow Automation

### PR Creation and Merge

When the user says "pr", "create pr", "submit pr", or similar, the Agent automatically:

1. `git push -u origin HEAD` — push the current branch
2. `gh pr create` — create PR with title and description
3. `gh pr checks --watch` — wait for CI checks to pass
4. `gh pr merge --squash --delete-branch` — squash merge and delete branch

If `gh` authentication fails, the Agent generates PR info (link, title, body) and provides commands for the user to execute manually.

### Version Upgrade and Release

When the user says "upgrade", "release", "tag", or similar:

1. **Version number**: use user-specified version if provided; otherwise determine from code changes:
   - Breaking changes or major features → bump major version
   - New features (backward compatible) → bump minor version
   - Bug fixes → bump patch version
2. **Diff check**: `git diff main...HEAD` or `git log` to review unmerged commits
3. **Release Note**: bilingual, Chinese first then English, one item per line:
   ```
   feat: feature description
   fix: fix description
   refactor: refactor description
   ...

   feature description in English
   fix description in English
   refactor description in English
   ```
4. **Tag push**: attempt `git tag v{x.y.z} && git push origin v{x.y.z}`; if auth fails, provide commands for the user

## Common Commands

```bash
go test ./...
go build -o knot-core cmd/core/main.go
go mod tidy
```

Recent state, bounded OSC7 observation and explicit SFTP cd/pause/resume follow
are implemented. Preserve success-only target history updates and raw PTY bytes.
Agent authentication and forwarding share the platform dialing abstraction;
Windows uses context-aware go-winio named pipes. File diagnostics have bounded
rotation, redaction and explicit lifecycle ownership. For the B09–B11 batch the
user explicitly deferred native non-Linux tests to CLI work; cross-build results
must never be presented as native platform runtime validation.
