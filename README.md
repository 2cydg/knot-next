# Knot

A modern SSH connection manager providing powerful automation capabilities through a local HTTP/WebSocket API.

## Overview

This repository contains **knot-core**: a local capability service that provides SSH/SFTP functionality through a standardized HTTP/WebSocket API.

knot-core is designed to be consumed by:
- Terminal clients (bilingual resource management and native SSH implemented; SFTP UI under development)
- Command-line scripts
- AI agents and automation tools
- Third-party applications
- Browser-based interfaces

## Architecture

```
Scripts / Terminal / AI / Third-party Clients
                  │
            HTTP/JSON + WS
                  │
             knot-core
                  │
      Config/secrets + SSH pool
      + session/SFTP/forward/...
                  │
          Remote SSH Hosts
```

## Requirements

- Go 1.27.1 or later
- Linux, macOS, or Windows
- Supported architectures: amd64, arm64

## Building from Source

```bash
# From the repository root
./scripts/build.sh
./scripts/test.sh
./scripts/lint.sh
# Build all six platform/architecture targets
./scripts/build.sh --all
```

## Installation

1. Start the core service:
   ```bash
   knot-core init
   knot-core --config ./config.toml
   ```

2. Use HTTP/WebSocket API to manage connections:
   ```bash
   # Check service status
   curl -H "Authorization: Bearer <token>" http://localhost:17898/v1/status
   
   # List servers
   curl -H "Authorization: Bearer <token>" http://localhost:17898/v1/config/servers
   ```

See [API documentation](core/docs/api/README.md) for complete API reference.

## Documentation

- [API Reference](core/docs/api/README.md) - HTTP/WebSocket API documentation
- [Architecture](core/docs/architecture.md) - System architecture and design
- [Security](core/docs/security.md) - Security model and best practices
- [Migration](core/docs/migration.md) - Migrating from legacy knot versions

## Project Status

The SSH/SFTP core baseline is implemented and its audit fixes include protocol validation, bounded resources and atomic credential persistence. The public API [workflow example](core/examples/baseline/README.md) and [CLI handoff](core/docs/cli-handoff.md) are available. Current validation and native platform boundaries are recorded in [acceptance evidence](core/docs/baseline-acceptance.md).

Current capabilities:
- ✅ Server configuration management
- ✅ SSH connection pooling with keep-alive
- ✅ SFTP file operations
- ✅ Key and proxy management
- ✅ Secrets encryption (platform-specific)
- ✅ Connection identities, dynamic SFTP follow, proxy probes and private-key original-byte import
- ✅ Local/remote/dynamic forwarding with temporary and server automatic rules
- 🚧 Broadcast mode (planned)
- 🚧 Configuration sync (planned)
- 🚧 TUI interface (bilingual resource CRUD, forwarding and native SSH implemented; SFTP UI under development)

The independent [CLI module](cli/README.md) supports local help/version and a
bilingual TUI with explicit core URL/Secret setup, resource CRUD, proxy tests, sync credentials,
forwarding management, offline recovery and native SSH with host trust and
authentication prompts. Start knot-core manually; SFTP file UI and transfers
remain in development. SSH exits return to the TUI when launched there;
`knot <alias>` / `knot ssh <alias>` exit with the remote status.
Root build/test/lint scripts process both modules. See the
[workspace stage record](../docs/cli-tui-progress.md) for delivered scope and validation.

## License

MIT License - see [LICENSE](LICENSE) for details.

## Contributing

See [AGENTS.md](AGENTS.md) for development guidelines and architecture details.

Recent target history (`state.json`), bounded OSC7 observation and explicit SFTP
follow control are available. SSH Agent supports Unix sockets and Windows named
pipes; file logs use redaction and bounded rotation. Linux protocol/HTTP/process
and race validation is complete for this batch. Native macOS/Windows Agent,
credential-store and terminal validation is deferred to CLI integration under
user authorization. See [API docs](core/docs/api/README.md) and
[acceptance evidence](core/docs/baseline-acceptance.md).

Core gap completion contracts and executed validation: [acceptance](core/docs/gap-acceptance.md), [public API scenarios](core/examples/gaps/README.md). SFTP UI and transfer integration remain next; cross-builds are compile evidence, with native platform verification recorded separately.
