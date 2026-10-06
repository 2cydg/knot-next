# Migration Guide

This document provides guidance for migrating from legacy knot installations to knot-core.

## Overview

knot-core uses a different configuration format and directory structure compared to legacy knot:

| Aspect | Legacy knot | knot-core |
|--------|------------|-----------|
| Config format | TOML | JSON |
| Config directory | `~/.config/knot/` or `$XDG_CONFIG_HOME/knot/` | `~/.config/knot-core/` or `$XDG_CONFIG_HOME/knot-core/` |
| Config file | `config.toml` | `config.json` |
| Keychain service (macOS) | `knot` / `knot-master-key` | `knot-core` / `knot-core-master-key` |
| Secret Service (Linux) | `knot` / `knot-master-key` | `knot-core` / `knot-core-master-key` |

## Migration API

knot-core provides built-in migration APIs to help transition from legacy configurations:

### Check Migration Status

```bash
GET /v1/config/migration
```

Returns whether a legacy configuration is detected and available for migration.

### Preview Migration Plan

```bash
GET /v1/config/migration/plan
```

Returns a detailed plan showing:
- Servers to be migrated
- Keys to be migrated
- Proxies to be migrated
- Conflicts with existing configuration
- Summary of changes

### Apply Migration

```bash
POST /v1/config/migration/apply
Content-Type: application/json

{
  "mode": "skip_existing"
}
```

Migration modes:
- `skip_existing`: Keep existing knot-core config, only add new items from legacy
- `overwrite`: Replace existing items with legacy configuration
- `fail_on_conflict`: Abort if any conflicts are detected

**Note**: The API does not automatically back up your configuration. You must manually back up your configurations before applying migration (see step 1 below).

## Migration Process

### 1. Backup Current Configuration

Before migrating, back up both your legacy and current configurations:

```bash
# Backup legacy config
cp -r ~/.config/knot ~/.config/knot.backup

# Backup current knot-core config (if exists)
cp -r ~/.config/knot-core ~/.config/knot-core.backup
```

### 2. Review Migration Plan

Use the migration API to preview what will be migrated:

```bash
curl -H "Authorization: Bearer <token>" \
  http://localhost:17898/v1/config/migration/plan
```

Review the plan carefully, especially:
- Server aliases that might conflict
- Key names and paths
- Proxy configurations
- Any warnings about data that cannot be automatically migrated

### 3. Apply Migration

Once you've reviewed the plan, apply the migration:

```bash
curl -X POST \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"mode": "skip_existing"}' \
  http://localhost:17898/v1/config/migration/apply
```

### 4. Verify Migration

After migration, verify your configuration:

```bash
curl -H "Authorization: Bearer <token>" \
  http://localhost:17898/v1/config/servers
```

Test connections to ensure everything works as expected.

## What Gets Migrated

### Servers

All server configurations are migrated, including:
- Host, port, user
- Aliases
- Jump hosts (converted to jump_host_id references)
- Proxy settings (converted to proxy_id references)
- Tags
- Authentication preferences

### Keys

SSH keys are migrated with:
- Paths to private key files
- Key types and metadata
- Passphrases (re-encrypted with new platform provider)

### Proxies

Proxy configurations including:
- SOCKS5 and HTTP proxies
- Host, port, authentication

### Secrets

Encrypted secrets are:
1. Decrypted using the legacy encryption provider
2. Re-encrypted using the knot-core encryption provider
3. Stored in the new format

The migration handles platform-specific encryption:
- macOS: Keychain access
- Linux: Secret Service or machine-id fallback
- Windows: DPAPI

## What Does NOT Get Migrated

The following legacy features require manual handling:

### Port Forwarding Rules

Legacy persistent forwarding rules are not automatically migrated. You will need to recreate them using the knot-core forwarding API once that feature is available.

### Usage History

The "last used" timestamps and usage history are not migrated. New usage tracking will start fresh.

### Known Hosts

The `known_hosts` file location may change. If you have a custom path configured, update your knot-core configuration to point to it. Otherwise, you may need to re-verify host keys on first connection.

### Custom Shell Hooks

If you used shell hooks for directory following or other automation, these need to be reconfigured for knot-core.

## Troubleshooting

### Migration Fails with Encryption Error

If migration fails to decrypt legacy secrets:

1. Ensure you can still access the legacy knot configuration
2. Verify platform keychain/secret service access:
   - macOS: Check Keychain Access.app for "knot" entries
   - Linux: Verify Secret Service is running
3. Try accessing legacy knot to ensure encryption is working
4. Check logs for specific error messages

### Alias Conflicts

If you have conflicting aliases between legacy and knot-core:

- Use `skip_existing` strategy to keep current knot-core configuration
- Use `overwrite` strategy to prefer legacy configuration
- Manually rename aliases before migration to avoid conflicts

### Reference Resolution Errors

If migration reports unresolved references (jump hosts, proxies):

1. Check the migration plan for details
2. Ensure all referenced servers and proxies exist in legacy config
3. Migration will create new IDs for references; existing knot-core IDs are preserved when using `skip_existing`

## Rolling Back

If you need to roll back after migration:

1. Stop knot-core
2. Restore from backup:
   ```bash
   rm -rf ~/.config/knot-core
   cp -r ~/.config/knot-core.backup ~/.config/knot-core
   ```
3. Restart knot-core

Your legacy configuration remains untouched during migration, so you can always return to using legacy knot if needed.

## Migration Limitations

### Platform Credential Store Changes

The migration re-encrypts secrets using the new platform provider names. This means:

- macOS Keychain entries will be created with new service names
- Linux Secret Service entries will use new attributes
- The legacy keychain entries are not automatically cleaned up

### Configuration Format Differences

Some configuration options may have different names or structures in JSON format. The migration attempts to map these automatically, but review the migrated configuration for any unexpected changes.

### Incomplete Feature Parity

If you use legacy features not yet available in knot-core (forwarding, broadcast, sync, archive), migration will succeed but those features won't be functional until implemented in knot-core.

## Best Practices

1. **Test in a clean environment first**: Create a test user or VM to try migration before applying to your main configuration
2. **Keep legacy config available**: Don't delete your legacy configuration until you've verified knot-core works for your use cases
3. **Review the plan**: Always review the migration plan before applying
4. **Back up both sides**: Keep backups of both legacy and knot-core configurations
5. **Incremental migration**: Consider migrating a few servers first, verify they work, then migrate the rest

## Getting Help

If you encounter issues during migration:

1. Check the knot-core logs for detailed error messages
2. Review the migration plan for warnings and conflicts
3. Consult the API documentation for manual configuration options
4. File an issue with:
   - Migration plan output (redact sensitive data)
   - Error messages from logs
   - Platform and version information
