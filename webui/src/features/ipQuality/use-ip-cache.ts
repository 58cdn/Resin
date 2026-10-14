import { useEffect, useRef, useState } from "react";
import { ApiError, apiRequest } from "../../lib/api-client";
import type { IPQualityResult, IPQualitySourceStatus } from "../nodes/ip-quality";
import { cacheReadErrorKey, isIPCacheResult, normalizeCacheIP } from "./cache";

type State = {
  phase: "idle" | "loading" | "done" | "error" | "canceled";
  ip?: string;
  error?: string;
  result?: IPQualityResult;
  sources?: IPQualitySourceStatus[];
  sourcesError?: boolean;
  receivedAt?: number;
};

export function useIPCache() {
  const [state, setState] = useState<State>({ phase: "idle" });
  const active = useRef<{ id: number; controller: AbortController; timer: ReturnType<typeof setTimeout> } | null>(null);
  const sequence = useRef(0);
  function stop() {
    sequence.current++;
    if (active.current) {
      clearTimeout(active.current.timer);
      active.current.controller.abort();
      active.current = null;
    }
  }
  useEffect(() => () => stop(), []);

  async function read(value: string) {
    if (active.current) return;
    const ip = normalizeCacheIP(value);
    if (!ip) return;
    const id = ++sequence.current;
    const controller = new AbortController();
    const timer = setTimeout(() => {
      if (sequence.current !== id) return;
      stop();
      setState({ phase: "error", ip, error: "缓存读取已超时，请稍后手动重试。" });
    }, 20_000);
    active.current = { id, controller, timer };
    setState({ phase: "loading", ip });
    try {
      const options = { signal: controller.signal, cache: "no-store" as const };
      const result = await apiRequest<unknown>(`/api/v1/ip-checks/${encodeURIComponent(ip)}`, options);
      if (sequence.current !== id) return;
      if (!isIPCacheResult(result, ip)) {
        setState({ phase: "error", ip, error: "缓存响应与查询不匹配或格式无效，已隐藏结果。" });
        return;
      }
      let sources: IPQualitySourceStatus[] | undefined;
      let sourcesError = false;
      try {
        const catalog = await apiRequest<{ items: IPQualitySourceStatus[] }>("/api/v1/ip-checks/sources", options);
        if (!catalog || !Array.isArray(catalog.items) || catalog.items.some(source => !source || typeof source.name !== "string" || typeof source.enabled !== "boolean" || typeof source.available !== "boolean" || (source.kind !== "by_ip" && source.kind !== "via_node") || (source.reason !== undefined && typeof source.reason !== "string") || (source.attribution !== undefined && typeof source.attribution !== "string"))) throw new Error("Invalid catalog");
        sources = catalog.items.filter(source => source.kind === "by_ip");
      } catch (error) {
        if (error instanceof ApiError && [401, 403, 503].includes(error.status)) throw error;
        if (controller.signal.aborted) throw error;
        sourcesError = true;
      }
      if (sequence.current === id) setState({ phase: "done", ip, result, sources, sourcesError, receivedAt: Date.now() });
    } catch (error) {
      if (sequence.current === id) setState({ phase: "error", ip, error: cacheReadErrorKey(error instanceof ApiError ? error.status : undefined) });
    } finally {
      clearTimeout(timer);
      if (active.current?.id === id) active.current = null;
    }
  }

  return {
    state, read,
    reset() { stop(); setState({ phase: "idle" }); },
    cancel() { stop(); setState({ phase: "canceled" }); },
  };
}
