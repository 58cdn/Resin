import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ApiError, apiRequest } from "../../lib/api-client";
import type { IPQualityCheckResult } from "../nodes/ip-quality";
import { CHECK_WAIT_MS, READ_WAIT_MS, isManualResult, manualErrorKey, nodeIdentity, normalizeSubject, sameNode, sourceCatalog, validSelection } from "./model";
import type { PreparedCheck, SubjectKind } from "./model";

type Operation = "prepare" | "check" | "cache";
type State = { phase: "idle" | "prepare" | "check" | "cache" | "ready" | "done" | "error" | "stopped"; prepared?: PreparedCheck; result?: IPQualityCheckResult; operation?: Operation; message?: string; receivedAt?: number };

export function useManualCheck() {
  const [state, setState] = useState<State>({ phase: "idle" });
  const queryClient = useQueryClient();
  const sequence = useRef(0);
  const preparedRef = useRef<PreparedCheck | undefined>(undefined);
  const active = useRef<{ id: number; controller: AbortController; timer: ReturnType<typeof setTimeout> } | null>(null);
  function stop() {
    sequence.current++;
    if (active.current) { clearTimeout(active.current.timer); active.current.controller.abort(); active.current = null; }
  }
  useEffect(() => () => stop(), []);

  async function run(operation: Operation, job: (signal: AbortSignal, current: () => boolean) => Promise<Partial<State>>) {
    if (active.current) return;
    const id = ++sequence.current, controller = new AbortController();
    const timer = setTimeout(() => {
      if (sequence.current !== id) return;
      stop();
      setState({ phase: "error", prepared: preparedRef.current, operation, message: operation === "check" ? "等待已超时；检测是否完成未知，可手动读取缓存核对。" : "读取已超时，请稍后手动重试。" });
    }, operation === "check" ? CHECK_WAIT_MS : READ_WAIT_MS);
    active.current = { id, controller, timer };
    setState({ phase: operation, operation, prepared: preparedRef.current });
    try {
      const outcome = await job(controller.signal, () => sequence.current === id);
      if (sequence.current !== id) return;
      if (outcome.prepared) preparedRef.current = outcome.prepared;
      setState({ phase: operation === "prepare" ? "ready" : "done", prepared: preparedRef.current, operation, receivedAt: Date.now(), ...outcome });
    } catch (error) {
      if (sequence.current === id) setState({ phase: "error", prepared: preparedRef.current, operation, message: error instanceof ValidationError ? error.message : manualErrorKey(error instanceof ApiError ? error.status : undefined, operation === "cache") });
    } finally {
      clearTimeout(timer);
      if (active.current?.id === id) active.current = null;
    }
  }

  return {
    state,
    reset() { stop(); preparedRef.current = undefined; setState({ phase: "idle" }); },
    clearResult() { if (!active.current) setState({ phase: "ready", prepared: preparedRef.current }); },
    cancel() { stop(); setState({ phase: "stopped", prepared: preparedRef.current, message: "已停止等待；已完成的缓存不回滚，共享检测可能继续。" }); },
    prepare(kind: SubjectKind, input: string) {
      const value = normalizeSubject(kind, input);
      if (!value || active.current) return;
      preparedRef.current = undefined;
      return run("prepare", async (signal, current) => {
        const node = kind === "node" ? nodeIdentity(await apiRequest<unknown>(`/api/v1/nodes/${value}`, { signal, cache: "no-store" }), value) : undefined;
        if (!current()) return {};
        if (kind === "node" && !node) throw new ValidationError("节点缺少有效出口或响应无效；请刷新对象，不会自动探测出口。");
        const sources = sourceCatalog(await apiRequest<unknown>("/api/v1/ip-checks/sources", { signal, cache: "no-store" }));
        if (!sources) throw new ValidationError("来源目录响应无效，不能执行检测。");
        return { prepared: { kind, value, ip: node?.ip ?? value, node: node ?? undefined, sources } };
      });
    },
    execute(names: string[], force: boolean, cache = false) {
      const prepared = preparedRef.current;
      if (!prepared || active.current || (!cache && !validSelection(prepared, names))) return;
      const selected = [...names];
      return run(cache ? "cache" : "check", async (signal, current) => {
        const options = { signal, cache: "no-store" as const };
        async function verifyNode() {
          if (prepared?.node) {
            const latest = nodeIdentity(await apiRequest<unknown>(`/api/v1/nodes/${prepared.value}`, options), prepared.value);
            if (!sameNode(prepared.node, latest)) throw new ValidationError("节点出口或版本已变化，已隐藏结果；请重新准备。");
          }
        }
        await verifyNode();
        if (!current()) return {};
        const path = prepared.kind === "node" ? `/api/v1/nodes/${prepared.value}/${cache ? "ip-check" : "actions/check-ip"}` : cache ? `/api/v1/ip-checks/${encodeURIComponent(prepared.value)}` : "/api/v1/ip-checks/lookup";
        let result: unknown;
        try {
          result = await apiRequest<unknown>(path, cache ? options : { ...options, method: "POST", body: { ...(prepared.kind === "ip" ? { ip: prepared.value } : {}), sources: selected, force } });
        } finally {
          // A failed/canceled POST may already have written individual sources.
          // Mark cached queries stale, without starting a read or a check.
          if (!cache) void queryClient.invalidateQueries({ queryKey: ["node-ip-quality"], refetchType: "none" });
        }
        if (!current()) return {};
        if (!isManualResult(result, prepared, cache ? undefined : selected)) throw new ValidationError("响应对象、来源或格式不匹配，已隐藏结果；缓存可能已更新。");
        await verifyNode();
        return { result };
      });
    },
  };
}

class ValidationError extends Error {}
