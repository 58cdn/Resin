#!/usr/bin/env python3
"""Example Resin plugin: append events to daily JSON Lines files.

It speaks the Resin plugin protocol directly (JSON-RPC 2.0, one JSON object
per line on stdin/stdout) using only the Python standard library, so it can
serve as a reference for plugins written in any language.

stdout is reserved for the protocol. Diagnostics go to stderr, which Resin
forwards to its own log.
"""

import datetime
import json
import os
import sys

SCHEMA_VERSION = 1
KNOWN_EVENTS = (
    "request.finished",
    "lease.created",
    "lease.replaced",
    "lease.removed",
    "lease.expired",
)


def log(msg):
    print(f"jsonl-exporter: {msg}", file=sys.stderr, flush=True)


def match_event(patterns, event_type):
    for p in patterns:
        if p == "*" or p == event_type:
            return True
        if p.endswith(".*") and event_type.startswith(p[:-1]):
            return True
    return False


class Exporter:
    def __init__(self):
        self.data_dir = ""
        self.events = ["*"]
        self.prefix = "events"
        self.directory = ""
        self.retention_days = 7
        self.current_day = None
        self.handle = None

    # --- protocol methods ---

    def register(self, params):
        self.data_dir = params.get("data_dir") or os.getcwd()
        self.configure(params)
        log(f"registered as {params.get('plugin_id')} (resin {params.get('resin_version')})")
        return {"schema_version": SCHEMA_VERSION, "name": "JSONL Event Exporter", "version": "1.0.0"}

    def configure(self, params):
        cfg = params.get("config") or {}
        if not isinstance(cfg, dict):
            raise ValueError("config must be an object")
        events = cfg.get("events") or ["*"]
        if not isinstance(events, list) or not all(isinstance(e, str) for e in events):
            raise ValueError("events must be a list of strings")
        for e in events:
            if not any(match_event([e], known) for known in KNOWN_EVENTS):
                raise ValueError(f"unknown event type {e!r}")
        prefix = (cfg.get("file_prefix") or "events").strip()
        if not prefix or any(c in prefix for c in "/\\:") or prefix.startswith("."):
            raise ValueError("file_prefix must be a plain file name")
        retention = cfg.get("retention_days", 7)
        if not isinstance(retention, int) or isinstance(retention, bool) or retention < 0:
            raise ValueError("retention_days must be a non-negative integer")
        directory = (cfg.get("directory") or "").strip() or self.data_dir
        os.makedirs(directory, exist_ok=True)

        # Only commit the new config once everything validated.
        self.close_file()
        self.events, self.prefix, self.directory, self.retention_days = events, prefix, directory, retention
        return {}

    def event_batch(self, params):
        written = 0
        for ev in params.get("events") or []:
            if not match_event(self.events, ev.get("type", "")):
                continue
            self.file().write(json.dumps(ev, ensure_ascii=False, separators=(",", ":")) + "\n")
            written += 1
        if written:
            self.handle.flush()
        return {}

    def shutdown(self, _params):
        self.close_file()
        return {}

    # --- files ---

    def file(self):
        today = datetime.datetime.now(datetime.timezone.utc).date()
        if self.handle is None or today != self.current_day:
            self.close_file()
            path = os.path.join(self.directory, f"{self.prefix}-{today.isoformat()}.jsonl")
            self.handle = open(path, "a", encoding="utf-8")
            self.current_day = today
            self.prune(today)
        return self.handle

    def close_file(self):
        if self.handle is not None:
            self.handle.close()
            self.handle = None

    def prune(self, today):
        if self.retention_days <= 0:
            return
        cutoff = today - datetime.timedelta(days=self.retention_days)
        head, tail = f"{self.prefix}-", ".jsonl"
        for name in os.listdir(self.directory):
            if not (name.startswith(head) and name.endswith(tail)):
                continue
            try:
                day = datetime.date.fromisoformat(name[len(head):-len(tail)])
            except ValueError:
                continue
            if day < cutoff:
                try:
                    os.remove(os.path.join(self.directory, name))
                except OSError as exc:
                    log(f"prune {name}: {exc}")


def main():
    exporter = Exporter()
    methods = {
        "plugin.register": exporter.register,
        "plugin.configure": exporter.configure,
        "event.batch": exporter.event_batch,
        "plugin.shutdown": exporter.shutdown,
    }
    out = sys.stdout.buffer

    def send(msg):
        out.write(json.dumps(msg, separators=(",", ":")).encode("utf-8") + b"\n")
        out.flush()

    for raw in sys.stdin.buffer:
        raw = raw.strip()
        if not raw:
            continue
        try:
            msg = json.loads(raw)
        except ValueError as exc:
            send({"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": f"parse error: {exc}"}})
            continue
        method, msg_id = msg.get("method"), msg.get("id")
        handler = methods.get(method)
        if handler is None:
            if msg_id is not None:
                send({"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32601, "message": f"method not found: {method}"}})
            continue
        try:
            result = handler(msg.get("params") or {})
        except Exception as exc:  # report every failure to the host
            if msg_id is not None:
                send({"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32603, "message": str(exc)}})
            continue
        if msg_id is not None:
            send({"jsonrpc": "2.0", "id": msg_id, "result": result})
        if method == "plugin.shutdown":
            break
    exporter.close_file()


if __name__ == "__main__":
    main()
