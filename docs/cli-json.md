# CLI JSON contract

`--json` selects machine output with `"schema_version":1`. Successful data is written only to stdout; errors and diagnostics are written only to stderr. Every envelope is one JSON object followed by a newline.

Single create/update success:

```json
{"schema_version":1,"resource":{"id":"CAPABILITY_ID","url":"https://whiteboard.example/whiteboards/markdown/CAPABILITY_ID","expires_at":1767229200,"permanent":false}}
```

Image upload always uses the plural envelope, even for one image, and preserves input order:

```json
{"schema_version":1,"resources":[{"id":"CAPABILITY_ID","url":"https://whiteboard.example/images/CAPABILITY_ID","expires_at":null,"permanent":true}]}
```

Delete and trusted-origin add/remove success is `{"schema_version":1}`. Trusted-origin list preserves insertion order:

```json
{"schema_version":1,"origins":["https://whiteboard.example"]}
```

Error output is stable:

```json
{"schema_version":1,"error":{"code":"not_found","message":"resource not found"}}
```

`expires_at` is nullable Unix seconds. `null` pairs with `permanent:true`; a timestamp pairs with `permanent:false`. URLs are resolved by the CLI against `--server`, because HTTP mutations return paths.

Timeout produces stderr `{"schema_version":1,"error":{"code":"timeout","message":"request timed out"}}` and exit 4. Cancellation uses code `canceled`.

| Exit | Meaning |
| ---: | --- |
| 0 | success |
| 1 | unexpected/internal failure |
| 2 | CLI usage or local configuration error |
| 3 | stable remote/domain error |
| 4 | timeout or cancellation |

Human mode prints URLs to stdout, one per line; successful delete prints nothing. Scripts should branch on `schema_version`, the top-level `resource`/`resources`/`error` member, and exit status. Version 1 will not change the meaning or type of existing fields; additive fields may be introduced. A breaking change requires a new schema version.
