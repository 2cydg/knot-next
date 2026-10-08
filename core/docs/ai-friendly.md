# Automation integration

Use the same authenticated public HTTP/WS API as other clients. Discover the local
runtime file and its token_path; call capabilities/health before selecting work.
Configuration changes go through core, including dedicated secrets endpoints.

Creation is asynchronous: wait for connected/open, handle explicit challenges,
and stop on unknown states. Transfer GET is authoritative after WS reconnect.
Exec responses distinguish remote exit status from framework and cleanup errors.
Record stable resource IDs and machine codes; redact credentials and do not
capture PTY/file contents in diagnostics.

See the [public workflow example](../examples/baseline/README.md), [API contract](api/README.md)
and [handoff checklist](cli-handoff.md). Do not infer unavailable forwarding,
sync, archive or update capabilities from placeholder packages.
