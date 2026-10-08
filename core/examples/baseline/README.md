# Public API workflow example

Start the daemon with `knot-core --allowed-origin http://localhost`. From `core/`, run:

```bash
go run ./examples/baseline -server prod -source /local/file -target /remote/new-file
```

The server profile must already exist through the config API. On first contact,
`-trust-new` explicitly permits accepting the host-key challenge; the client
prints the fingerprint when acceptance is required. Supply authentication through
`KNOT_PASSWORD`, `KNOT_KEY_ID` or `KNOT_PASSPHRASE` in the process environment.
Do not put credentials in command arguments or source code.

The client discovers `{XDG_STATE_HOME}/knot/runtime/core.json` (fallback
`~/.local/state/knot/runtime/core.json`), then reads `token_path`. `-runtime` can
select an explicitly configured daemon. The demo uses an allowed
`http://localhost` Origin; add it to a custom allowlist when required.

The workflow creates an SSH session, waits through auth/host-key challenges,
remembers a verified password/key selection, attaches and receives the snapshot,
runs bounded `true` exec, opens SFTP, lists `/`, uploads without overwrite,
receives the transfer snapshot and uses GET until terminal state, then closes
both sessions. It never edits config files. The upload is an intentional remote
write; choose a new destination. Key retries are persisted only as key IDs;
passphrases stay in memory. Exec uses the saved selection after the asynchronous
Remember write is observable.

HTTP timeout is 15 seconds, WS dial 5 seconds and read/write 10 seconds; the whole
workflow has a two-minute budget and interrupt cancellation. Cleanup uses fresh
five-second contexts. Errors and unknown states stop the workflow. For a real
terminal, use opcode-aware streaming and terminal restoration as described in
[CLI handoff](../../docs/cli-handoff.md); this example only reads the attach
snapshot and does not enter raw terminal mode.

`TestBaselineWorkflowThroughPublicAPI` runs this same client against real local
SSH/SFTP and HTTP/WS with static test crypto and artificial credentials. The
fixture alone composes internal services; operations go through public APIs.
