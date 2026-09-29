# Resin plugin examples

| Directory | Language | Capability | What it does |
| --- | --- | --- | --- |
| [`rate-limit`](rate-limit) | Go (`pkg/pluginsdk`) | `request_hook` | Token-bucket rate limit per account / client IP / platform, rejects with 429 |
| [`jsonl-exporter`](jsonl-exporter) | Python 3 (stdlib only) | `events` | Appends `request.finished` and `lease.*` events to daily JSON Lines files |
| [`index.example.json`](index.example.json) | — | marketplace | Example marketplace index listing both plugins |

See [doc/plugins.md](../../doc/plugins.md) for the full protocol and manifest reference.

External plugins run as child processes with the same OS privileges as Resin,
so they are disabled unless `RESIN_EXTERNAL_PLUGINS_ENABLED=true`.

## Build a package

A plugin package is a `.zip` or `.tar.gz` whose root (or single top-level
directory) contains `plugin.json` plus whatever the runtime command needs.

### Go: rate-limit

```bash
mkdir -p dist/example.rate-limit/bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/example.rate-limit/bin/rate-limit ./examples/plugins/rate-limit
cp examples/plugins/rate-limit/plugin.json dist/example.rate-limit/
tar -C dist -czf dist/example.rate-limit-1.0.0-linux-amd64.tar.gz example.rate-limit
```

On Windows build `bin/rate-limit.exe`; Resin appends `.exe` automatically when
the manifest command has no extension.

### Python: jsonl-exporter

```bash
cd examples/plugins/jsonl-exporter && zip ../../../dist/example.jsonl-exporter-1.0.0.zip plugin.json main.py
```

## Install

Any of:

- WebUI → **Plugins → Installed → Upload Plugin**.
- `curl -X POST -H "Authorization: Bearer $RESIN_ADMIN_TOKEN" --data-binary @dist/example.rate-limit-1.0.0-linux-amd64.tar.gz http://127.0.0.1:2260/api/v1/plugins/actions/upload`
- Copy the unpacked directory to `$RESIN_PLUGIN_DIR/<plugin id>/` and click **Rescan**.
- Publish the archives next to an index like `index.example.json` (with real
  `sha256` values from `sha256sum`) and add the index URL to
  `RESIN_PLUGIN_MARKETPLACE_URLS`.

Plugins are installed disabled. Enable and configure them on the Plugins page.
