# Knot Development Guide

## Repository Structure

This repository contains two independent Go modules: **knot-core** and **knot-cli**. The bilingual CLI provides resource CRUD, proxy tests, forwarding management and native SSH through the public API. SFTP UI and file transfers are still under development. Stage status is recorded in the workspace [docs/cli-tui-progress.md](../docs/cli-tui-progress.md).

Current structure:
- `core/` - knot-core service (module: knot-core)
- `cli/` - Frontend module: entry/i18n, typed public core client, connection setup and root TUI

## Architecture

Knot-core is a local capability service that provides SSH/SFTP functionality through HTTP/WebSocket APIs.

### Core Service (knot-core)

The core service is a local HTTP server that:
- Manages SSH connection pools
- Handles SFTP sessions
- Stores encrypted configuration and secrets
- Provides REST and WebSocket APIs
- Runs on localhost (default port 17898)

Key packages:
- `pkg/core/` - Core service initialization
- `pkg/config/` - Configuration management
- `pkg/session/` - SSH session management
- `pkg/sftp/` - SFTP operations
- `pkg/sshpool/` - SSH connection pooling
- `pkg/crypto/` - Platform-specific encryption

### CLI Client (under development)

The future CLI client will:
- Connect to knot-core via HTTP/WebSocket
- Provide TUI for server management
- Bridge native terminal for SSH connections
- Not directly modify configuration files

The new client does not migrate legacy shell/Cobra completion or SFTP REPL
completion. TUI search, resource selection, file browsing, and any input
suggestions are designed in the client phase. Core retains generic resource
queries, directory listing, glob matching, and directory caching through its API;
it should not implement legacy command syntax or completion display formatting.

Business flows use the public core HTTP/WS protocol and must not import core packages. The UI stack is pinned to Bubble Tea v2.1.0, Bubbles v2.2.1 and Lip Gloss v2.0.6, with coder/websocket v1.8.15. Root TUI state is only mutated in Update; requests carry page/generation and modal completions carry modal/instance identity. Keep exclusive confirmations ahead of all other keys. Do not restore Cobra, legacy completion or an SFTP REPL.

## Building

Build both modules:

```bash
# From repository root
./scripts/build.sh

# Or manually
cd core
go build -o ../bin/knot-core ./cmd/core
```

## Testing

Run tests for both modules (the workspace root is not a Go module):

```bash
# Test core and cli
./scripts/test.sh

# Or manually
cd core
go test ./... -v
```

## Development Workflow

1. Start core in development mode:
   ```bash
   ./bin/knot-core init
   knot-core --config ./config.toml
   ```

2. Test with HTTP/WebSocket API:
   ```bash
   # Check status
   curl -H "Authorization: Bearer <token>" http://localhost:17898/v1/status
   
   # List servers
   curl -H "Authorization: Bearer <token>" http://localhost:17898/v1/config/servers
   ```

## API Documentation

See [core/docs/api/README.md](core/docs/api/README.md) for complete API reference.

## Current Status

The SSH/SFTP core baseline is implemented, including asynchronous sessions,
challenge/retry, raw PTY attach, exec, transfers and recovery, bounded resources,
legacy TOML/crypto import, recent/CWD/follow, Agent and diagnostic logs. See
`core/docs/baseline-acceptance.md` for current evidence and `core/docs/cli-handoff.md`
for the public API handoff. Connection identities, mutable cross-connection follow, proxy tests, original-byte
key import and real L/R/D forwarding are also implemented. CLI/TUI, broadcast, archive,
sync, update, task, automatic shell hooks and log-tail API remain future work.
Native macOS/Windows credential-store, Agent, ACL and terminal verification is
explicitly deferred to CLI; six-platform builds are compile evidence only.

## Code Organization

### Core (`core/`)

```
core/
├── cmd/core/           # Main entry point
├── internal/
│   ├── api/            # HTTP/WS handlers
│   ├── auth/           # Token authentication
│   ├── paths/          # Path management
│   └── runtime/        # In-memory instance identity
├── pkg/
│   ├── config/         # Configuration
│   ├── crypto/         # Encryption
│   ├── session/        # SSH sessions
│   ├── sftp/           # SFTP operations
│   ├── sshpool/        # Connection pooling
│   ├── forward/        # Local/remote/dynamic forwarding resources
│   ├── broadcast/      # Broadcast mode (planned)
│   └── sync/           # Config sync (planned)
└── docs/               # Documentation
```

### CLI (`cli/`)

See [cli/README.md](cli/README.md) for connection setup, interactions and launch/exit contracts. Resource management, forwarding, SSH waiting/challenges and raw terminal bridging use real core responses; SFTP UI remains pending. Connection attempts outlive renderers; Bubble Tea must finish before the native bridge starts, and terminal input workers must join before returning to TUI. Challenge answers bind their displayed ID and invalidate prior polls. CLI Agent forwarding is best effort: skip it when core reports unavailable and silently retry once without it if setup fails; do not prompt for optional Agent availability. Keep host trust and authentication confirmations intact. Resource writes remain sequential under the user’s scope: do not add partial-save recovery machinery, rollback or unknown-outcome reconciliation. `scripts/build.sh`, `scripts/test.sh` and `scripts/lint.sh` process both modules with GOWORK=off; `build.sh --all` builds both for six CGO=0 targets. Native macOS/Windows terminal behavior still needs runtime validation.

## Contributing

Read [MEMORY.md](MEMORY.md) for persistent project-specific preferences.

When contributing:
1. Follow existing code style and patterns
2. Add tests for new functionality
3. Update documentation
4. Ensure the core module builds and tests successfully
5. Keep core focused on API capabilities, not UI
6. **Commit messages must strictly follow [Conventional Commits](https://www.conventionalcommits.org/) specification**
   - Format: `<type>(<scope>): <description>`
   - Types: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`
   - Example: `feat(session): add SSH keepalive support`
   - Example: `fix(api): correct migration request schema`

## Security

- Core service binds to localhost only
- Token-based authentication
- Platform-specific secret encryption (Keychain/Secret Service/DPAPI)
- Business secrets are encrypted; the restart-only service Secret is stored in the private core TOML file

## Migration from Legacy knot

See [core/docs/migration.md](core/docs/migration.md) for migration guidance from the original knot version.

B06 resource retention and cleanup are implemented: active capacity is separate from
terminal history, subscribers are bounded, and shutdown waits for owned workers.
See `core/docs/baseline-acceptance.md` for validation and platform boundaries.

Legacy TOML/crypto reuse, transient private-key passphrases, and explicit TOML
import are implemented (B07/B08). Normal legacy upgrades use config.toml directly;
JSON import is unsupported and existing JSON is preserved. Native macOS/Windows
credential-store interoperability remains unexecuted. See the migration and
baseline acceptance documents for exact behavior and validation.

## Validation boundaries

Use the acceptance record for actual test results. A timed-out SSH channel open
cannot be withdrawn through x/crypto/ssh; late workers remain tracked and retain
shared client leases until the protocol settles or transport closes. Do not
claim a public timeout means cleanup already finished. Native non-Linux runtime
validation and a formal terminal client remain outside this baseline batch.

Core startup uses only TOML service settings via `knot-core --config <file>`.
`knot-core init` creates a local config with port 17898 and a random 32-character
alphanumeric Secret without starting services or overwriting existing files.
CLI stores URL/Secret/auto_start/executable/config_file in client.toml, gates first
use on validated setup, and may launch a detached core on connection refusal.
It never reads core config, runtime files or token files. Empty Secret disables
Secret authentication; nonempty Secret protects all business HTTP/WS endpoints,
including version. Origin validation stays independent. Instance identity lives
in memory and remains API-visible. Explicit config paths govern writes and locks;
config-directory instance locks also protect shared crypto/trust state.

Default data directories use the user home (`.config/knot` and `.local/state/knot`); XDG directory environment variables do not override them. Explicit core --config retains its config-directory state layout.
