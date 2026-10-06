# Knot

A modern SSH connection manager providing powerful automation capabilities through a local HTTP/WebSocket API.

## Overview

This repository contains **knot-core**: a local capability service that provides SSH/SFTP functionality through a standardized HTTP/WebSocket API.

knot-core is designed to be consumed by:
- Terminal clients (TUI interface - planned)
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

- Go 1.26.2 or later
- Linux, macOS, or Windows
- Supported architectures: amd64, arm64

## Building from Source

```bash
# Build knot-core
cd core
go build -o ../bin/knot-core ./cmd/core

# Or use the build script
./scripts/build.sh
```

## Installation

1. Start the core service:
   ```bash
   knot-core --port 17898
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

This is a work in progress. The project is being refactored from the original knot codebase.

Current capabilities:
- ✅ Server configuration management
- ✅ SSH connection pooling with keep-alive
- ✅ SFTP file operations
- ✅ Key and proxy management
- ✅ Secrets encryption (platform-specific)
- 🚧 Port forwarding (planned)
- 🚧 Broadcast mode (planned)
- 🚧 Configuration sync (planned)
- 🚧 TUI interface (planned)

## License

MIT License - see [LICENSE](LICENSE) for details.

## Contributing

See [AGENTS.md](AGENTS.md) for development guidelines and architecture details.
