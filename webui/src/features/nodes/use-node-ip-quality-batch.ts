import { useEffect, useMemo, useRef, useState } from "react";
import { ApiError } from "../../lib/api-client";
import { readNodeIPQualityBatch } from "./ip-quality-api";
import { nextBatchExpiryDelay, readIPQualityBatches, type QualityCacheEntry, type QualityNode } from "./ip-quality-batch";

type BatchState = {
  scope: { key: string } | null;
  phase: "idle" | "reading" | "done" | "canceled" | "error";
  entries: Record<string, QualityCacheEntry>;
  completed: number;
  updatedAt: number;
  errorKey: string;
};
const EMPTY: BatchState = { scope: null, phase: "idle", entries: {}, completed: 0, updatedAt: 0, errorKey: "" };

function errorKey(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 429) return "读取过于频繁，请稍后重新读取本页缓存。";
    if (error.status === 503) return "IP 质量插件未启用或暂不可用。";
    if (error.status === 401 || error.status === 403) return "无权读取 IP 质量结果，请检查管理员登录状态。";
  }
  if (error instanceof Error && error.name === "TimeoutError") return "缓存读取超时，后续读取已停止。";
  return "本页缓存读取中断，未完成的节点保持未知。";
}

export function useNodeIPQualityBatch(nodes: QualityNode[], pageScope: string, enabled: boolean) {
  const scopeKey = JSON.stringify([pageScope, enabled, nodes.map((node) => [node.node_hash, node.egress_ip, node.last_egress_update])]);
  // A -> B -> A creates a new identity: returning to an old page must not revive
  // its completed facts or a canceled run's stale "reading" phase.
  const scope = useMemo(() => ({ key: scopeKey }), [scopeKey]);
  const total = new Set(nodes.map((node) => node.node_hash)).size;
  const [stored, setStored] = useState(EMPTY);
  const [clock, setClock] = useState(Date.now);
  const running = useRef<{ scope: typeof scope; controller: AbortController } | null>(null);
  const state = stored.scope === scope ? stored : EMPTY;
  const now = Math.max(clock, state.updatedAt);

  useEffect(() => () => {
    const run = running.current;
    if (run?.scope === scope) {
      running.current = null;
      run.controller.abort();
    }
  }, [scope]);

  useEffect(() => {
    const delay = nextBatchExpiryDelay(state.entries, now, Date.now());
    if (delay === null) return;
    const timer = setTimeout(() => setClock(Date.now()), delay);
    return () => clearTimeout(timer);
  }, [state.entries, now]);

  const start = async () => {
    if (!enabled || !total || running.current?.scope === scope) return;
    running.current?.controller.abort();
    const run = { scope, controller: new AbortController() };
    running.current = run;
    setStored({ ...EMPTY, scope, phase: "reading", updatedAt: Date.now() });
    try {
      await readIPQualityBatches(nodes, run.controller.signal, readNodeIPQualityBatch, (entries) => {
        if (running.current !== run) return;
        setStored((previous) => ({ ...previous, entries: { ...previous.entries, ...entries }, completed: previous.completed + Object.keys(entries).length, updatedAt: Date.now() }));
      });
      if (running.current === run) setStored((previous) => ({ ...previous, phase: "done" }));
    } catch (error) {
      if (running.current !== run) return;
      const unavailable = error instanceof ApiError && [401, 403, 503].includes(error.status);
      setStored((previous) => ({ ...previous, phase: "error", errorKey: errorKey(error), entries: unavailable ? {} : previous.entries, completed: unavailable ? 0 : previous.completed }));
    } finally {
      if (running.current === run) running.current = null;
    }
  };

  const cancel = () => {
    const run = running.current;
    if (!run || run.scope !== scope) return;
    running.current = null;
    run.controller.abort();
    setStored((previous) => ({ ...previous, phase: "canceled" }));
  };

  return { ...state, now, total, enabled, start, cancel };
}
