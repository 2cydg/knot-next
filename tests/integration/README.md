# Integration Tests

This directory is reserved for integration tests that verify knot-core functionality with real SSH/SFTP servers and test clients.

## Test Structure

Integration tests should:
- Use real SSH/SFTP servers (containerized or test fixtures)
- Test complete workflows end-to-end
- Verify API contracts with actual HTTP/WebSocket clients
- Cover platform-specific behaviors

## Running Integration Tests

```bash
# Run integration tests
cd tests/integration
go test ./...
```

## Test Categories

- **API Tests**: Verify HTTP/WebSocket API contracts
- **Session Tests**: Test SSH session lifecycle with real servers
- **SFTP Tests**: Test file operations with real SFTP servers
- **Migration Tests**: Test configuration migration with real legacy configs
- **Platform Tests**: Platform-specific encryption and authentication

## TODO

Integration tests are planned but not yet implemented. Priority areas:
1. SSH session state transitions with delayed handshake
2. SFTP transfer completion and event delivery
3. Configuration migration with real legacy fixtures
4. Platform-specific secret encryption
