# Configuration

Default path: `~/.agent-whiteboard/config.yaml`. Use global `--config PATH` to select another file.

## Preserve existing configuration

Inspect an existing file before editing it. Change only fields required for the requested setup. Do not replace an unknown configuration with a minimal example.

The file is strict YAML with `version: 1`. It must be a regular non-symlink and must not be writable by group or others. Use `0600` for a new file and `0700` for a new parent directory.

## Publishing server selection

The CLI chooses where to publish in this order:

1. `--server URL`;
2. non-empty `AGENT_WHITEBOARD_SERVER`;
3. YAML `client.server`;
4. `http://127.0.0.1:8567`.

`client.server` is the remote or local server used by publishing commands. `server.host` and `server.port` configure a server process started on this machine; they are not a remote-server discovery mechanism.

```yaml
version: 1

client:
  server: https://whiteboard.example
  timeout: 30s
```

If the user did not provide an origin and no environment or YAML value exists, ask for the publishing origin when localhost is not intended. Never guess a host.

## Enable Page Agent in a local viewer

For a new local all-in-one configuration:

```yaml
version: 1

viewer:
  local_agent:
    enabled: true
```

When a file already exists, merge only `viewer.local_agent.enabled: true`. Restart the publishing server after changing viewer settings.

A reader cannot enable Page Agent on a remote publishing server. If the control is absent remotely, report that the server operator must enable it.

For fields outside these setup tasks, use the repository's `docs/configuration.md` rather than copying the complete schema into the skill.
