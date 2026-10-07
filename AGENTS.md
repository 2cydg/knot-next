# Knot Development Guide

## Repository Structure

This repository currently contains the **knot-core** service as a single Go module. The CLI component will be developed in a future phase (M7) with a new TUI-based design.

Current structure:
- `core/` - knot-core service (module: knot-core)
- `cli/` - Reserved for future CLI development (empty, `.gitkeep` placeholder)

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

### Future CLI Client (planned for M7)

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

This design is planned but not yet implemented. The current phase focuses on completing the core service capabilities.

## Building

Build the core service:

```bash
# From repository root
./scripts/build.sh

# Or manually
cd core
go build -o ../bin/knot-core ./cmd/core
```

## Testing

Run tests for the core module:

```bash
# Test core
./scripts/test.sh

# Or manually
cd core
go test ./... -v
```

## Development Workflow

1. Start core in development mode:
   ```bash
   ./bin/knot-core --port 17898
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

This project is being refactored from the original knot codebase. The current phase (M0-M6) focuses on:

1. **M0: Migration baseline** ✅ (current)
   - Core code migrated
   - Build and test infrastructure
   - Documentation

2. **M1-M6: Core capabilities** (planned)
   - M1: Session state machine and API contracts
   - M2: Migration and service baseline
   - M3: Port forwarding
   - M4: Broadcast mode
   - M5: Archive and sync
   - M6: Update backend

3. **M7-M8: CLI and release** (future)
   - TUI design and development
   - Complete product packaging

See the project documentation for current capabilities and known issues.

## Code Organization

### Core (`core/`)

```
core/
├── cmd/core/           # Main entry point
├── internal/
│   ├── api/            # HTTP/WS handlers
│   ├── auth/           # Token authentication
│   ├── paths/          # Path management
│   └── runtime/        # Runtime file management
├── pkg/
│   ├── config/         # Configuration
│   ├── crypto/         # Encryption
│   ├── session/        # SSH sessions
│   ├── sftp/           # SFTP operations
│   ├── sshpool/        # Connection pooling
│   ├── forward/        # Port forwarding (planned)
│   ├── broadcast/      # Broadcast mode (planned)
│   └── sync/           # Config sync (planned)
└── docs/               # Documentation
```

### CLI (`cli/`)

Currently empty (`.gitkeep` placeholder). Will be populated in M7 with the new TUI-based CLI design.

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
- Configuration contains no plaintext secrets by default

## Migration from Legacy knot

See [core/docs/migration.md](core/docs/migration.md) for migration guidance from the original knot version.

B06 resource retention and cleanup are implemented: active capacity is separate from
terminal history, subscribers are bounded, and shutdown waits for owned workers.
See `core/docs/baseline-acceptance.md` for validation and platform boundaries.

## Known Issues

The following issues are documented for future phases:

- **P0**: SSH/SFTP async session state handling
- **P0**: SFTP transfer event completion detection
- **P0**: exec command timeout alignment
- **P1**: SFTP directory following
- **P1**: Legacy config migration platform compatibility
- **P1**: Windows Agent support

These will be addressed in M1-M2 phases. See project documentation for details.
