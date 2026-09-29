# Resin Plugins

[中文](plugins.zh-CN.md)

Plugins extend Resin without forking it. A plugin can:

- **inspect proxy requests** before routing: reject them, rewrite the routing
  identity (Platform / Account), or set/remove upstream request headers;
- **receive events**: finished requests (`request.finished`) and sticky lease
  lifecycle changes (`lease.created`, `lease.replaced`, `lease.removed`,
  `lease.expired`).

There are two kinds of plugins, both managed on the WebUI **Plugins** page and
through `/api/v1/plugins`:

| Kind | Where it runs | Availability |
| --- | --- | --- |
| **Builtin** | Compiled into Resin, in-process | Always available, disabled by default |
| **Package** | Child process started by Resin, any language | Only when `RESIN_EXTERNAL_PLUGINS_ENABLED=true` |

Package plugins are installed by uploading an archive, by copying a directory
into the plugin directory, or from a **plugin marketplace** (a static JSON
index served over HTTP).

## Builtin plugins

| ID | Capability | Description |
| --- | --- | --- |
| `resin.access-control` | request hook | Ordered allow/deny rules by client CIDR, platform, account (wildcards), target host (exact, glob, CIDR, `<local>`) and proxy type |
| `resin.header-rewrite` | request hook | Set/remove upstream headers per rule; values may use `${account}`, `${platform}`, `${client_ip}`, `${target_host}` |
| `resin.webhook` | events | POSTs event batches as `{"source":"resin","events":[...]}` to an HTTP endpoint |

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `RESIN_PLUGIN_DIR` | `<RESIN_STATE_DIR>/plugins` | Package plugins, one directory per plugin id. Plugin data lives in `<RESIN_PLUGIN_DIR>/.data/<id>` |
| `RESIN_EXTERNAL_PLUGINS_ENABLED` | `false` | Allow package plugins, uploads and the marketplace |
| `RESIN_PLUGIN_MARKETPLACE_URLS` | empty | Marketplace index URLs, separated by `;`, `,` or newlines |

Per-plugin settings (stored in `state.db`, editable at runtime):

| Setting | Default | Description |
| --- | --- | --- |
| `enabled` | `false` | Newly discovered or installed plugins start disabled |
| `priority` | `0` | Request hooks run from highest to lowest priority (ties by id). Range -10000..10000 |
| `timeout_ms` | `1000` | Per-request hook timeout. Range 10..60000 |
| `fail_closed` | `false` | When the hook errors or times out: `false` skips the plugin, `true` rejects the request with `503 PLUGIN_ERROR` |
| `config` | manifest defaults | Plugin-specific JSON object, validated against `config_fields` and hot-applied |

The `resin.webhook` builtin has its own `config.timeout_ms`; it defaults to
5000 and is capped at 10000 milliseconds because event delivery has a bounded
shutdown budget.


A package plugin runs with the same OS user and privileges as Resin. Only
install plugins you trust.

- Package plugins, uploads and the marketplace are **off by default**.
- Plugin processes do **not** inherit `RESIN_*` environment variables (which
  include the admin and proxy tokens). They receive only `RESIN_PLUGIN_ID`,
  `RESIN_PLUGIN_DIR` and `RESIN_PLUGIN_DATA_DIR`.
- `Proxy-Authorization` is never passed to plugins. Plugins cannot modify `Host`, `Content-Length`, `Transfer-Encoding`, `Connection`, `Upgrade`, `TE`,
  `Trailer` or `Proxy-Authorization`; invalid header names and values are
  ignored. CONNECT and SOCKS5 requests never apply header operations.
- Events never contain request/response bodies or headers.
- Archives are checked for path traversal, absolute paths, links and size
  limits (100 MiB download, 512 MiB extracted, 4096 entries). Marketplace
  artifacts must match their `sha256`.
- Marketplace URLs are shown with credentials and query strings redacted.

## The request hook chain

For every HTTP forward, CONNECT, reverse proxy and SOCKS5 request, after proxy
authentication and before routing, Resin calls each enabled request plugin in
priority order:

1. The plugin receives a `RequestInfo`. Platform, Account and headers reflect
   changes made by higher-priority plugins. When no platform is supplied, the
   plugin sees `Platform: "Default"`.
2. A `reject` decision stops the chain. HTTP clients get the plugin's status
   (default 403) and message, with `X-Resin-Error: PLUGIN_REJECTED` and
   `X-Resin-Plugin: <plugin id>`. SOCKS5 clients get reply `0x02`
   (connection not allowed).
3. A `continue` decision may override Platform/Account (used for routing and
   request logs) and set/remove headers. Header changes are applied to the
   upstream request for reverse proxy and plain HTTP forward proxy only.
   Reverse proxy uses Rewrite semantics: it does not append `X-Forwarded-For`,
   and a plugin cannot expose the client IP by setting or removing that header.
4. If the client disconnects while a plugin is running, the request ends
   silently: no response is written and the plugin is not counted as an error.
5. Errors and timeouts are counted in the plugin stats. With `fail_closed`
   the request fails with `503 PLUGIN_ERROR` (SOCKS5: general failure);
   otherwise the plugin is skipped. An enabled fail-closed plugin that cannot
   start publishes the same 503 placeholder in the request chain.

When no request plugin is enabled the hot path costs a single atomic load.

## Events

Events are delivered asynchronously and never slow down proxying. Each
subscribed plugin has a bounded queue of 4096 events; events are sent in
batches of up to 256, or every second. When a queue is full, new events are
dropped and counted in `events_dropped`. The webhook builtin only queues event
patterns selected by its `events` configuration.

`request.finished` data:

```json
{
  "started_at": "2026-09-28T08:00:00.123Z",
  "proxy_type": "reverse",
  "client_ip": "10.0.0.8",
  "platform_id": "8b1c...",
  "platform_name": "openai",
  "account": "user_tom",
  "target_host": "api.example.com",
  "target_url": "https://api.example.com/v1/models",
  "node_hash": "9f2c...e1a0",
  "node_tag": "sub-A/HK-01",
  "egress_ip": "1.2.3.4",
  "duration_ns": 183000000,
  "first_byte_ns": 95000000,
  "net_ok": true,
  "http_method": "GET",
  "http_status": 200,
  "ingress_bytes": 512,
  "egress_bytes": 4096
}
```

Failed requests also carry `resin_error`, `upstream_stage` and
`upstream_err_msg`.

`lease.*` data:

```json
{
  "platform_id": "8b1c...",
  "platform_name": "openai",
  "account": "user_tom",
  "node_hash": "9f2c...e1a0",
  "egress_ip": "1.2.3.4",
  "created_at": "2026-09-28T08:00:00Z"
}
```

## Writing a package plugin

A package is a directory (or a `.zip` / `.tar.gz` of it) with a `plugin.json`
manifest at its root, or inside a single top-level directory.

```
example.rate-limit/
├── plugin.json
└── bin/
    └── rate-limit
```

### Manifest (`plugin.json`)

```json
{
  "schema_version": 1,
  "id": "example.rate-limit",
  "name": "Rate Limit",
  "version": "1.0.0",
  "description": "Token-bucket rate limit per account.",
  "author": "you",
  "homepage": "https://example.com",
  "license": "MIT",
  "min_resin_version": "1.0.0",
  "capabilities": {
    "request_hook": true,
    "events": ["request.finished", "lease.*"]
  },
  "config_fields": [
    {"name": "rate_per_second", "label": "Requests per second", "type": "number", "default": 10, "required": true}
  ],
  "runtimes": {
    "linux-amd64": {"command": ["bin/rate-limit"]},
    "any": {"command": ["python3", "main.py"], "env": {"PYTHONUNBUFFERED": "1"}}
  }
}
```

| Field | Description |
| --- | --- |
| `schema_version` | Must be `1` |
| `id` | 1-64 chars of `a-z 0-9 . _ -`, starting and ending with a letter or digit. Must equal the directory name. `resin.*` ids of builtins are reserved |
| `name`, `version` | Required. Dotted numeric versions (`1.2.3`, optional `v` prefix, `-suffix` ignored) are compared numerically to detect marketplace updates; other strings are compared for equality |
| `min_resin_version` | Optional dotted version. Packages requiring a newer Resin are refused at install/load time. Development builds (`dev`) skip the check |
| `capabilities.request_hook` | Receive `request.inspect` calls |
| `capabilities.events` | Event subscriptions: exact types, `*`, or `prefix.*` such as `lease.*` |
| `config_fields` | Schema of the top-level keys of the config object; the WebUI renders a form from it. Builtin plugins reject undeclared keys |
| `runtimes` | `<goos>-<goarch>` (e.g. `linux-arm64`, `windows-amd64`) or `any`. A relative `command[0]` (containing `/` or `\`, or starting with `.`) is resolved inside the package and may not escape it (on Windows `.exe` is appended when the file has no extension and the `.exe` exists); an absolute path is used as is; a bare name such as `python3` is looked up in `PATH`. The working directory is the package directory |

`config` is an object: missing or `null` fields with manifest defaults are filled
before Configure. Builtin plugins reject undeclared top-level keys; package
plugins receive the object defined by their protocol implementation.

| Type | JSON value | WebUI control |
| --- | --- | --- |
| `string` | string | text input |
| `secret` | string | password input |
| `text` | string | textarea |
| `number` | number | number input |
| `integer` | integer | number input |
| `boolean` | boolean | switch |
| `enum` | one of `enum_values` | select |
| `string_list` | array of strings | textarea, one item per line |
| `json` | any JSON | JSON editor |

### Protocol

Resin starts the process and exchanges [JSON-RPC 2.0](https://www.jsonrpc.org/specification)
messages with it, **one JSON object per line** (NDJSON): host → plugin on
stdin, plugin → host on stdout. Everything written to stderr is copied to the
Resin log. Never print anything else on stdout.

Requests may be interleaved: `request.inspect` calls are sent concurrently and
responses can be returned in any order (match them by `id`).

| Method | Params | Result | Notes |
| --- | --- | --- | --- |
| `plugin.register` | `{schema_version, resin_version, plugin_id, data_dir, config}` | `{schema_version: 1, name?, version?, capabilities?}` | First call. Apply `config` before replying; return an error to refuse to start. `capabilities` may narrow (never widen) the manifest |
| `plugin.configure` | `{config}` | `{}` | Hot config update. On error keep the previous config; the admin sees the message |
| `request.inspect` | `RequestInfo` | `RequestDecision` | Only with `request_hook`. Must answer within the admin-configured timeout |
| `event.batch` | `{events: [{type, time, data}]}` | `{}` | Batches are sent one at a time, in order |
| `plugin.shutdown` | `{}` | `{}` | Release resources and exit. Stdin is closed afterwards and the process is killed if it does not exit within 3 s |

`RequestInfo`:

```json
{
  "proxy_type": "forward | reverse | socks5",
  "is_connect": false,
  "client_ip": "10.0.0.8",
  "platform": "openai",
  "account": "user_tom",
  "target_host": "api.example.com",
  "method": "POST",
  "url": "https://api.example.com/v1/chat/completions",
  "headers": {"User-Agent": ["curl/8.0"]}
}
```

`RequestDecision` (every field optional; `{}` means continue unchanged):

```json
{
  "action": "continue | reject",
  "status": 429,
  "message": "Rate limit exceeded",
  "platform": "openai-backup",
  "account": "user_tom",
  "set_headers": {"X-Tenant": "tom"},
  "remove_headers": ["X-Debug"]
}
```

A full exchange:

```
→ {"jsonrpc":"2.0","id":1,"method":"plugin.register","params":{"schema_version":1,"resin_version":"1.2.0","plugin_id":"example.rate-limit","data_dir":"/var/lib/resin/plugins/.data/example.rate-limit","config":{"rate_per_second":10}}}
← {"jsonrpc":"2.0","id":1,"result":{"schema_version":1}}
→ {"jsonrpc":"2.0","id":2,"method":"request.inspect","params":{"proxy_type":"reverse","platform":"openai","account":"tom","target_host":"api.example.com"}}
← {"jsonrpc":"2.0","id":2,"result":{"action":"reject","status":429,"message":"Rate limit exceeded"}}
→ {"jsonrpc":"2.0","id":3,"method":"plugin.shutdown","params":{}}
← {"jsonrpc":"2.0","id":3,"result":{}}
```

If the process exits unexpectedly Resin restarts it with exponential backoff
(1 s up to 30 s) and replays `plugin.register` with the current config.
Calls made while the plugin is restarting fail fast and follow `fail_closed`.

### Go SDK

Go plugins can use `github.com/Resinat/Resin/pkg/pluginsdk`, which only
depends on the standard library:

```go
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

type blocker struct{ host string }

func (b *blocker) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg struct{ Host string `json:"host"` }
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	b.host = cfg.Host
	return nil
}

func (b *blocker) InspectRequest(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	if req.TargetHost == b.host {
		return pluginsdk.Reject(403, "blocked"), nil
	}
	return nil, nil // continue
}

func main() {
	if err := pluginsdk.Serve(&blocker{}); err != nil {
		log.Fatal(err)
	}
}
```

Implement `pluginsdk.RequestInspector` for request hooks,
`pluginsdk.EventHandler` for events, and optionally `pluginsdk.Initializer`
(receives `RegisterParams`, including `DataDir`) and `pluginsdk.Shutdowner`.
`InspectRequest` is called concurrently. Build with `CGO_ENABLED=0` for a
self-contained binary.

Complete examples live in [`examples/plugins`](../examples/plugins):
a Go rate limiter and a dependency-free Python event exporter.

## Installing

- **Upload**: WebUI → Plugins → *Upload Plugin*, or
  `POST /api/v1/plugins/actions/upload` with the raw archive as the body.
- **Directory**: copy the package to `$RESIN_PLUGIN_DIR/<id>/` and click
  *Rescan* (`POST /api/v1/plugins/actions/rescan`). Rescan also restarts
  plugins whose `plugin.json` changed and removes deleted ones.
- **Marketplace**: WebUI → Plugins → *Marketplace* → *Install*.

Installing a new version of an installed plugin replaces its files but keeps
its settings and data directory. If the plugin is enabled and the new version
fails to start, the old version is restored. Uninstalling removes the package,
its data directory and its settings.

## Marketplace index

A marketplace is any HTTP(S) URL serving an index document. Several indexes
can be configured; when two list the same id, the first URL wins.

```json
{
  "schema_version": 1,
  "name": "My plugins",
  "plugins": [
    {
      "id": "example.rate-limit",
      "name": "Rate Limit",
      "version": "1.0.0",
      "description": "Token-bucket rate limit per account.",
      "author": "you",
      "homepage": "https://example.com",
      "license": "MIT",
      "tags": ["security"],
      "min_resin_version": "1.0.0",
      "artifacts": [
        {"os": "linux", "arch": "amd64", "url": "example.rate-limit-1.0.0-linux-amd64.tar.gz", "sha256": "<hex>", "size": 1234567},
        {"os": "any", "arch": "any", "url": "https://cdn.example.com/example.rate-limit-1.0.0.zip", "sha256": "<hex>"}
      ]
    }
  ]
}
```

- Artifact URLs may be relative to the index URL, so a GitHub Pages site or
  an object storage bucket with `index.json` next to the archives works. A
  relative artifact path does not inherit the index URL's query string; the
  API listing redacts artifact credentials and query parameters, while install
  requests retain the resolved private URL internally.
- The best artifact for the running platform is chosen: exact `os`/`arch`,
  then `os` with `arch: "any"`, then `any`/`any`.
- `sha256` is required and verified before extraction. The archive's
  `plugin.json` id must equal the index id.
- Entries whose `min_resin_version` is newer than the running Resin are
  listed but not installable.

See [`examples/plugins/index.example.json`](../examples/plugins/index.example.json).

## HTTP API

All endpoints require the admin token.

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/v1/plugins` | List plugins (paginated: `limit`, `offset`) |
| `GET` | `/api/v1/plugins/{id}` | Get one plugin |
| `PATCH` | `/api/v1/plugins/{id}` | Update `enabled`, `priority`, `timeout_ms`, `fail_closed`, `config` (config replaces the whole object) |
| `DELETE` | `/api/v1/plugins/{id}` | Uninstall a package plugin (`409` for builtins) |
| `POST` | `/api/v1/plugins/actions/rescan` | Rescan the plugin directory |
| `POST` | `/api/v1/plugins/actions/upload` | Install a `.zip` / `.tar.gz` sent as the request body (max 100 MiB) |
| `GET` | `/api/v1/plugin-marketplace` | List plugins from all marketplace indexes with install state |
| `POST` | `/api/v1/plugin-marketplace/{id}/actions/install` | Install or upgrade a marketplace plugin |

Uploads and marketplace endpoints return `409 CONFLICT` while external
plugins are disabled; package plugins are then not loaded at all, so their
`GET`/`PATCH`/`DELETE` return `404`.
