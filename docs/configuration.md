# Configuration

`agent-whiteboard` uses one strict, versioned YAML configuration file. The default path is `~/.agent-whiteboard/config.yaml`; global `--config PATH` selects a different file and may appear before or after subcommands.

A missing default file is equivalent to `version: 1` with all built-in values. An explicitly selected file must exist when loading configuration for a foreground command. `agent serve --daemon` records the selected absolute path and permits its final file to be absent; the managed child loads it when it starts. Relative `--config` paths are resolved from the current working directory. `~` and `~/...` are supported; named-user forms such as `~other/...` are not.

## Precedence

For client and server settings, resolution is:

1. an explicitly supplied CLI flag;
2. a non-empty `AGENT_WHITEBOARD_*` environment variable;
3. YAML;
4. the built-in value.

An empty environment value does not erase YAML. A supplied empty flag is still an explicit value and normally fails validation. The `viewer` section and agent trusted-origin/default-access settings do not have environment or flag overrides. Foreground `agent serve` supports the agent overrides listed below.

| YAML | Environment | Flag |
| --- | --- | --- |
| `client.server` | `AGENT_WHITEBOARD_SERVER` | `--server` |
| `client.timeout` | `AGENT_WHITEBOARD_TIMEOUT` | `--timeout` |
| `server.host` | `AGENT_WHITEBOARD_HOST` | `serve --host` |
| `server.port` | `AGENT_WHITEBOARD_PORT` | `serve --port` |
| `server.storage` | `AGENT_WHITEBOARD_STORAGE` | `serve --storage` |
| `server.cleanup_interval` | `AGENT_WHITEBOARD_CLEANUP_INTERVAL` | `serve --cleanup-interval` |
| `server.default_expires_in` | `AGENT_WHITEBOARD_DEFAULT_EXPIRES_IN` | `serve --default-expires-in` |
| `server.shutdown_timeout` | `AGENT_WHITEBOARD_SHUTDOWN_TIMEOUT` | `serve --shutdown-timeout` |
| `server.log_mode` | `AGENT_WHITEBOARD_LOG_MODE` | `serve --log-mode` |
| `server.max_whiteboard_bytes` | `AGENT_WHITEBOARD_MAX_WHITEBOARD_BYTES` | `serve --max-whiteboard-bytes` |
| `server.max_context_bytes` | `AGENT_WHITEBOARD_MAX_CONTEXT_BYTES` | `serve --max-context-bytes` |
| `server.max_image_bytes` | `AGENT_WHITEBOARD_MAX_IMAGE_BYTES` | `serve --max-image-bytes` |
| `server.max_image_request_bytes` | `AGENT_WHITEBOARD_MAX_IMAGE_REQUEST_BYTES` | `serve --max-image-request-bytes` |
| `agent.port` | `AGENT_WHITEBOARD_AGENT_PORT` | `agent serve --port` |
| `agent.provider_idle_timeout` | `AGENT_WHITEBOARD_PROVIDER_IDLE_TIMEOUT` | `agent serve --provider-idle-timeout` |
| `agent.shutdown_timeout` | `AGENT_WHITEBOARD_AGENT_SHUTDOWN_TIMEOUT` | `agent serve --shutdown-timeout` |
| — | `AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE` | `agent serve --pi-executable` |
| — | `AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE` | `agent serve --codex-executable` |

A relative YAML `server.storage` path is resolved against the directory containing that YAML file. Relative environment and flag storage paths are resolved by the server process from its current working directory.

## Complete version 1 schema

All fields are optional except top-level `version`:

```yaml
version: 1

client:
  server: https://whiteboard.example
  timeout: 30s

server:
  host: 127.0.0.1
  port: 8567
  storage: data
  cleanup_interval: 15m
  default_expires_in: 86400
  shutdown_timeout: 10s
  log_mode: console
  max_whiteboard_bytes: 10485760
  max_context_bytes: 1048576
  max_image_bytes: 26214400
  max_image_request_bytes: 104857600

viewer:
  local_agent:
    enabled: false

agent:
  port: 8568
  trusted_origins:
    - https://whiteboard.example
  provider_idle_timeout: 60m
  shutdown_timeout: 10s
  default_access: configured
```

The current release validates the complete schema, uses `client` and `server` for publication behavior, supports editing `agent.trusted_origins`, and provides foreground `agent serve` for the local agent API. On macOS, `agent serve --daemon` installs or updates and starts a per-user LaunchAgent; `agent daemon status|restart|stop|uninstall` manages it. `stop` retains the plist and `uninstall` removes it. Managed daemon operations are unsupported on Linux, where foreground `agent serve` is the supported mode.

Pi and Codex executable selection is intentionally not stored in YAML. Foreground `agent serve` uses an explicit provider flag first, then the matching non-empty environment variable, then resolves `pi` or `codex` from `PATH`. An explicitly supplied empty executable flag is invalid. A missing provider executable marks only that provider unavailable; it does not prevent the broker or the other provider from starting.

Pi and Codex inherit the foreground process environment so providers use the normal effective user home, authentication, tools, extensions, apps, hooks, skills, approval policy, sandbox, project trust, and other native configuration. Agent Whiteboard does not set a production `CODEX_HOME`, edit provider configuration files, copy authentication, or override approval, sandbox, tools, or other native settings. Page Agent builds Model, Effort, skill, and manual-compaction controls from each provider's live capabilities; Codex also exposes Standard/Fast Speed, while Pi omits it and limits selection to one skill per message. Existing and restored conversations inherit their effective native tuple. A genuinely new conversation may use that provider's same-origin last-accepted preference. Applying Pi settings follows normal Pi behavior: it changes the current Page Agent Pi session and Pi's future default, not already-running Pi sessions. App Server experimental APIs remain disabled for Codex.

With `--daemon`, `--pi-executable` and `--codex-executable` and their matching environment variables select executable paths during installation. The foreground-only `--port`, `--provider-idle-timeout`, and `--shutdown-timeout` flags are rejected because the managed child reads them from Agent Whiteboard configuration at startup. The LaunchAgent records the absolute Agent Whiteboard and configuration paths and resolved provider executable paths; it does not copy Pi or Codex configuration, authentication, or the installing shell's ambient environment. Managed providers therefore use the same effective default user configuration as foreground providers, without editing provider configuration files.

Durations use Go duration syntax and must be positive. `server.port` accepts 0–65535; YAML `agent.port` accepts 1–65535, while `agent serve --port` also accepts 0 for an ephemeral listener. `default_expires_in` and byte limits are nonnegative integers. Zero default expiration means permanent resources. A zero byte limit selects that limit's built-in value; it does not disable the limit. The effective image request limit cannot be smaller than the effective per-image limit. `log_mode` is `console` or `json`, and `default_access` is `configured`. Existing `content-only` values remain parseable for compatibility but do not create a provider sandbox.

| Setting | Built-in |
| --- | ---: |
| Client server | `http://127.0.0.1:8567` |
| Client timeout | `30s` |
| Server host | `127.0.0.1` |
| Server port | `8567` |
| Server storage | `$HOME/.agent-whiteboard` |
| Cleanup interval | `15m` |
| Default expiration | `86400` seconds |
| Shutdown timeout | `10s` |
| Log mode | `console` |
| Whiteboard source limit | 10 MiB |
| Creator context limit | 1 MiB |
| Image limit | 25 MiB each |
| Image request limit | 100 MiB |
| Viewer local agent enabled | `false` |
| Agent port | `8568` |
| Trusted origins | empty |
| Provider idle timeout | `60m` |
| Agent shutdown timeout | `10s` |
| Agent default access | `configured` |

## Strict parsing and file safety

The file must contain exactly one YAML mapping document with `version: 1`. Unknown fields, duplicate keys, aliases, merge keys, non-string mapping keys, unsupported versions, and values of the wrong YAML type are rejected. YAML numbers and booleans must not be quoted. Durations and other strings may use ordinary plain or quoted YAML string syntax.

The configuration target must be an unchanged regular file, not a symlink, and must not be writable by group or others. Existing files may be group/other-readable, although `0600` is recommended. Loading configuration does not require an owner-only parent directory.

Trusted-origin list and edit operations have stronger rules on macOS and Linux: the immediate configuration parent must be a real directory, not a symlink, and must not be writable by group or others. The target is opened without following a symlink and is checked for replacement races. Atomic edits use an advisory lock, an owner-only temporary file, file sync, rename, and directory sync. A missing default parent is created as `0700`, and a missing default file created by `agent trust add` is `0600`. Explicitly selected files are never created.

Trusted-origin editing and listing are supported only on macOS and Linux. Other platforms return an unsupported-platform configuration error. Managed agent daemon lifecycle operations are macOS-only; Linux returns explicit foreground guidance.

## Pre-production Page Agent v4 upgrade

Page Agent v4 uses a format-neutral exact `source` context field for Markdown and HTML while retaining the same API and WebSocket version. It replaces v3 in place and does not migrate older broker conversation/workspace state. Before deploying the v4 viewer and broker, stop the foreground broker or managed daemon and clear the disposable Page Agent conversation/workspace state under the configured Agent Whiteboard storage. Deploy viewer and broker together, then restart `agent serve` or the daemon. Do not delete public whiteboards or provider-native Pi/Codex histories: they are outside this reset. Mixed v3/v4 viewers and brokers fail closed rather than negotiating or adapting.

## Trusted HTTPS origins

```sh
agent-whiteboard agent trust add https://whiteboard.example
agent-whiteboard agent trust list
agent-whiteboard --json agent trust list
agent-whiteboard agent trust remove https://whiteboard.example
```

Configured origins must be exact HTTPS origins: scheme, host, and optional port only. User information, paths (including `/`), queries, fragments, wildcards, and HTTP origins are rejected. Host names are canonicalized to lowercase ASCII, IP addresses are canonicalized, and explicit port 443 is removed. Add and remove are idempotent; list preserves insertion order. Human add/remove output is silent, while human list prints one canonical origin per line.

The broker separately and automatically admits canonical browser origins on literal `http://127.0.0.1`, with an optional valid non-default port. This runtime exception is not stored in `agent.trusted_origins` and does not appear in `agent trust list`. It excludes `localhost`, other IPv4 loopback spellings and addresses, IPv6 loopback, explicit default port 80, and every non-loopback HTTP origin.

JSON add/remove output is:

```json
{"schema_version":1}
```

JSON list output is:

```json
{"schema_version":1,"origins":["https://whiteboard.example"]}
```
