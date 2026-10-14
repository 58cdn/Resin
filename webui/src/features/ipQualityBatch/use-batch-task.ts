import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ApiError, apiRequest } from "../../lib/api-client";
import type { IPQualitySourceStatus } from "../nodes/ip-quality";
import { selectable, sourceCatalog } from "../ipQualityManual/model";
import { batchErrorKey, isTerminal, normalizeBatchHashes, parseBatchTask } from "./model";
import type { BatchEnvelope, BatchTask } from "./model";

type Operation = "prepare" | "submit" | "poll" | "cancel";
type State = {
  operation?: Operation; hashes?: string[]; sources?: IPQualitySourceStatus[];
  envelope?: BatchEnvelope; taskID?: string; task?: BatchTask;
  observing: boolean; message?: string; receivedAt?: number;
};
export const BATCH_READ_MS = 20_000;
export const BATCH_POLL_MS = 2_000;
export const BATCH_OBSERVE_MS = 300_000;

export function useBatchTask() {
  const [state, setState] = useState<State>({ observing: false });
  const current = useRef(state);
  const queryClient = useQueryClient();
  const sequence = useRef(0);
  const active = useRef<{ id: number; controller: AbortController; timer: ReturnType<typeof setTimeout> } | null>(null);
  const pollTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const budgetTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  function publish(value: State) { current.current = value; setState(value); }
  function stop() {
    sequence.current++;
    clearTimeout(pollTimer.current); clearTimeout(budgetTimer.current);
    pollTimer.current = undefined; budgetTimer.current = undefined;
    if (active.current) { clearTimeout(active.current.timer); active.current.controller.abort(); active.current = null; }
  }
  useEffect(() => () => stop(), []);
  function pause(message = "已暂停页面观察；服务端任务可能继续，暂停不等于取消。") {
    const operation = current.current.operation;
    stop();
    publish({ ...current.current, operation: undefined, observing: false,
      message: operation === "submit" ? "未能确认任务是否接受；请保留原提交，重试可能再次执行。" : operation === "cancel" ? "取消请求未获确认；任务可能仍在运行，请查询原任务。" : message });
  }
  function beginBudget() {
    clearTimeout(budgetTimer.current);
    budgetTimer.current = setTimeout(() => pause("本轮五分钟观察已结束；可手动继续查询，服务端任务可能继续。"), BATCH_OBSERVE_MS);
  }
  function schedule() {
    clearTimeout(pollTimer.current);
    if (current.current.observing && current.current.taskID && current.current.task && !isTerminal(current.current.task)) {
      pollTimer.current = setTimeout(() => { void read(true); }, BATCH_POLL_MS);
    }
  }
  async function run(operation: Operation, job: (signal: AbortSignal) => Promise<Partial<State>>) {
    if (active.current) return;
    const id = ++sequence.current, controller = new AbortController();
    clearTimeout(pollTimer.current);
    const timer = setTimeout(() => {
      if (sequence.current !== id) return;
      pause(operation === "prepare" ? "读取来源已超时，请稍后手动重试。" : "任务请求已超时；已暂停查询，服务端任务状态未知。");
    }, BATCH_READ_MS);
    active.current = { id, controller, timer };
    publish({ ...current.current, operation, message: undefined });
    let succeeded = false;
    try {
      const outcome = await job(controller.signal);
      if (sequence.current !== id) return;
      const terminal = outcome.task && isTerminal(outcome.task);
      if (terminal) clearTimeout(budgetTimer.current);
      publish({ ...current.current, ...outcome, operation: undefined, observing: terminal ? false : current.current.observing, receivedAt: Date.now() });
      succeeded = true;
    } catch (error) {
      if (sequence.current !== id) return;
      clearTimeout(budgetTimer.current);
      publish({ ...current.current, task: undefined, operation: undefined, observing: false,
        message: error instanceof InvalidResponse ? error.message : batchErrorKey(error instanceof ApiError ? error.status : undefined, operation) });
    } finally {
      clearTimeout(timer);
      if (active.current?.id === id) active.current = null;
      if (sequence.current === id && succeeded) schedule();
    }
  }
  async function read(observe: boolean) {
    const { taskID, envelope } = current.current;
    if (!taskID || active.current) return;
    publish({ ...current.current, observing: observe });
    return run("poll", async signal => {
      const task = parseBatchTask(await apiRequest<unknown>(`/api/v1/ip-checks/tasks/${taskID}`, { signal, cache: "no-store" }), envelope, taskID);
      if (!task) throw new InvalidResponse("任务响应与提交范围或标识不匹配，已隐藏结果并暂停查询。");
      return { task };
    });
  }
  async function send(envelope: BatchEnvelope) {
    if (active.current || current.current.taskID) return;
    // Persist the exact request in memory before sending. No automatic POST retry.
    publish({ ...current.current, envelope, task: undefined, observing: true });
    beginBudget();
    return run("submit", async signal => {
      let value: unknown;
      try { value = await apiRequest<unknown>("/api/v1/ip-checks/actions/check-batch", { method: "POST", body: envelope, signal, cache: "no-store" }); }
      finally { void queryClient.invalidateQueries({ queryKey: ["node-ip-quality"], refetchType: "none" }); }
      const task = parseBatchTask(value, envelope);
      if (!task) throw new InvalidResponse("接受响应无效；任务可能已执行。保留原提交，重试可能再次执行。");
      return { task, taskID: task.id };
    });
  }
  return {
    state,
    reset() { stop(); publish({ observing: false }); },
    pause,
    async prepare(input: string) {
      const hashes = normalizeBatchHashes(input);
      if (!hashes || active.current || current.current.envelope || current.current.taskID) return;
      stop(); publish({ observing: false });
      return run("prepare", async signal => {
        const sources = sourceCatalog(await apiRequest<unknown>("/api/v1/ip-checks/sources", { signal, cache: "no-store" }));
        if (!sources) throw new InvalidResponse("来源目录响应无效，不能执行检测。");
        return { hashes, sources };
      });
    },
    submit(names: string[], force: boolean) {
      const { hashes, sources, envelope, taskID } = current.current;
      if (!hashes || !sources || envelope || taskID || active.current || !names.length || new Set(names).size !== names.length
        || !names.every(name => sources.some(source => source.name === name && selectable(source, "node")))) return;
      const bytes = new Uint8Array(16);
      crypto.getRandomValues(bytes);
      return send({ request_id: [...bytes].map(byte => byte.toString(16).padStart(2, "0")).join(""), node_hashes: [...hashes], sources: [...names], force });
    },
    retry() { if (current.current.envelope && !current.current.taskID) return send(current.current.envelope); },
    restore(input: string) {
      const taskID = input.trim().toLowerCase();
      if (!/^[a-f\d]{32}$/.test(taskID) || active.current) return;
      stop(); publish({ taskID, observing: false });
      return read(false);
    },
    refresh() { if (active.current) return; clearTimeout(budgetTimer.current); return read(false); },
    observe() { if (active.current || !current.current.taskID || current.current.task && isTerminal(current.current.task)) return; beginBudget(); return read(true); },
    async cancel() {
      const { taskID, envelope, task, operation } = current.current;
      if (!taskID || task && isTerminal(task) || operation === "cancel" || operation === "submit" || operation === "prepare") return;
      // Abort and invalidate an older poll before a cancel response can be shown.
      stop(); publish({ ...current.current, operation: undefined, observing: true }); beginBudget();
      return run("cancel", async signal => {
        const value = await apiRequest<unknown>(`/api/v1/ip-checks/tasks/${taskID}/actions/cancel`, { method: "POST", signal, cache: "no-store" });
        const response = parseBatchTask(value, envelope, taskID);
        if (!response) throw new InvalidResponse("取消响应无效；请查询原任务，不能确认任务已取消。");
        return { task: response };
      });
    },
  };
}
class InvalidResponse extends Error {}
