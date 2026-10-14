import { normalizeManualIP } from "../ipQualityManual/model.ts";

export type BatchEnvelope = { request_id: string; node_hashes: string[]; sources: string[]; force: boolean };
export type BatchTaskState = "queued" | "running" | "canceling" | "completed" | "canceled";
export type BatchItemState = "queued" | "running" | "completed" | "failed" | "canceled";
export type BatchObservationStatus = "all_ok" | "partial_failure" | "all_failed" | "unavailable" | "no_observations";
export type BatchProgress = { total: number; queued: number; running: number; completed: number; failed: number; canceled: number };
export type BatchError = { code: "NOT_FOUND" | "CONFLICT" | "SERVICE_UNAVAILABLE" | "INTERNAL"; message: string };
export type BatchSource = {
  source: string;
  kind: "by_ip" | "via_node";
  ok: boolean;
  status: "ok" | "error" | "unavailable";
  checked_at: string;
  expires_at: string;
  error?: string;
};
export type BatchItem = {
  node_hash: string;
  state: BatchItemState;
  started_at?: string;
  finished_at?: string;
  egress_ip?: string;
  checked_at?: string;
  observation_status?: BatchObservationStatus;
  sources?: BatchSource[];
  error?: BatchError;
};
export type BatchTask = {
  id: string;
  request_id: string;
  state: BatchTaskState;
  sources: string[];
  force: boolean;
  created_at: string;
  finished_at?: string;
  cancel_reason?: "requested" | "deadline" | "engine_stopped";
  progress: BatchProgress;
  items: BatchItem[];
};

const sourceKinds: Record<string, BatchSource["kind"]> = {
  "db-ip": "by_ip", "egress-consistency": "via_node", "cloudflare-challenge": "via_node", "ai-region": "via_node",
  ippure: "via_node", scamalytics: "by_ip", ipdata: "by_ip", "cloudflare-radar": "by_ip",
};
const itemStates = ["queued", "running", "completed", "failed", "canceled"] as const;
const taskStates = ["queued", "running", "canceling", "completed", "canceled"] as const;
const errorCodes = ["NOT_FOUND", "CONFLICT", "SERVICE_UNAVAILABLE", "INTERNAL"] as const;
const cancelReasons = ["requested", "deadline", "engine_stopped"] as const;
const encoder = new TextEncoder();
const object = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const member = <T extends string>(values: readonly T[], value: unknown): value is T => typeof value === "string" && values.includes(value as T);
const hash = (value: unknown): value is string => typeof value === "string" && /^[a-f\d]{32}$/.test(value);
const requestID = (value: unknown): value is string => typeof value === "string" && /^[a-z\d_-]{1,64}$/i.test(value);
const text = (value: unknown, maxBytes: number): value is string => typeof value === "string" && value.length <= maxBytes && encoder.encode(value).length <= maxBytes;

// Go emits RFC3339Nano. Do not let Date.parse accept date-only strings or
// silently normalize impossible calendar dates such as February 30.
function timestamp(value: unknown): value is string {
  if (typeof value !== "string" || value.length > 40) return false;
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$/.exec(value);
  if (!match) return false;
  const [, year, month, day, hour, minute, second, zone] = match;
  const y = Number(year), m = Number(month), d = Number(day);
  const days = [31, y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return m >= 1 && m <= 12 && d >= 1 && d <= days[m - 1] && Number(hour) <= 23 && Number(minute) <= 59 && Number(second) <= 59
    && (zone === "Z" || (Number(zone.slice(1, 3)) <= 23 && Number(zone.slice(4)) <= 59)) && Number.isFinite(Date.parse(value));
}

function sourceNames(value: unknown): value is string[] {
  return Array.isArray(value) && value.length > 0 && value.length <= 8 && new Set(value).size === value.length
    && Array.from(value).every(name => typeof name === "string" && Object.hasOwn(sourceKinds, name));
}

function sameSet(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every(value => right.includes(value));
}

/** The raw nonempty line limit is checked before duplicates are removed. */
export function normalizeBatchHashes(input: string): string[] | null {
  if (typeof input !== "string" || input.length > 16 * 1024) return null;
  const lines = input.split(/\r\n?|\n/).map(line => line.trim()).filter(Boolean);
  if (!lines.length || lines.length > 50 || lines.some(line => !/^[a-f\d]{32}$/i.test(line))) return null;
  return [...new Set(lines.map(line => line.toLowerCase()))];
}

function parseSource(value: unknown): BatchSource | null {
  if (!object(value) || typeof value.source !== "string" || !Object.hasOwn(sourceKinds, value.source) || value.kind !== sourceKinds[value.source]
    || typeof value.ok !== "boolean" || !member(["ok", "error", "unavailable"] as const, value.status)
    || value.ok !== (value.status === "ok") || !timestamp(value.checked_at) || !timestamp(value.expires_at)
    || Date.parse(value.expires_at) <= Date.parse(value.checked_at) || (value.error !== undefined && !text(value.error, 512))) return null;
  return { source: value.source, kind: value.kind as BatchSource["kind"], ok: value.ok, status: value.status,
    checked_at: value.checked_at, expires_at: value.expires_at, ...(value.error !== undefined ? { error: value.error as string } : {}) };
}

function observation(sources: BatchSource[]): BatchObservationStatus {
  if (!sources.length) return "no_observations";
  const ok = sources.filter(source => source.ok && source.status === "ok").length;
  if (ok === sources.length) return "all_ok";
  if (ok > 0) return "partial_failure";
  return sources.every(source => source.status === "unavailable") ? "unavailable" : "all_failed";
}

function parseItem(value: unknown, names: string[], createdAt: string): BatchItem | null {
  if (!object(value) || !hash(value.node_hash) || !member(itemStates, value.state)) return null;
  const item: BatchItem = { node_hash: value.node_hash, state: value.state };
  if (value.started_at !== undefined) {
    if (!timestamp(value.started_at) || Date.parse(value.started_at) < Date.parse(createdAt)) return null;
    item.started_at = value.started_at;
  }
  if (value.finished_at !== undefined) {
    if (!timestamp(value.finished_at) || Date.parse(value.finished_at) < Date.parse(item.started_at ?? createdAt)) return null;
    item.finished_at = value.finished_at;
  }
  if (value.state === "queued" ? item.started_at !== undefined || item.finished_at !== undefined
    : value.state === "running" ? item.started_at === undefined || item.finished_at !== undefined
      : item.finished_at === undefined || (value.state !== "canceled" && item.started_at === undefined)) return null;

  if (value.state !== "completed") {
    if (value.egress_ip !== undefined || value.checked_at !== undefined || value.observation_status !== undefined
      || (value.sources !== undefined && (!Array.isArray(value.sources) || value.sources.length !== 0))) return null;
    if (value.state === "failed") {
      if (!object(value.error) || !member(errorCodes, value.error.code) || !text(value.error.message, 512) || !value.error.message) return null;
      item.error = { code: value.error.code, message: value.error.message };
    } else if (value.error !== undefined) return null;
    return item;
  }

  if (value.error !== undefined || typeof value.egress_ip !== "string" || value.egress_ip.length > 45 || !timestamp(value.checked_at)) return null;
  // This validates literal syntax; the server remains authoritative for its
  // public-address policy. No URL, path, hostname or scoped IPv6 is accepted.
  const ip = normalizeManualIP(value.egress_ip);
  if (!ip || (value.sources !== undefined && (!Array.isArray(value.sources) || value.sources.length > 8))) return null;
  const sources: BatchSource[] = [];
  for (const raw of value.sources ?? []) {
    const parsed = parseSource(raw);
    if (!parsed || sources.some(source => source.source === parsed.source)) return null;
    sources.push(parsed);
  }
  if (value.observation_status !== observation(sources) || (sources.length > 0 && !sameSet(sources.map(source => source.source), names))) return null;
  // Cache observations may predate the task or already be expired. They are
  // historical metadata, so neither Date.now() nor created_at is a freshness gate.
  if (sources.length && Date.parse(value.checked_at) !== Math.max(...sources.map(source => Date.parse(source.checked_at)))) return null;
  return { ...item, egress_ip: ip, checked_at: value.checked_at, observation_status: observation(sources), sources };
}

/** Validate ownership and internal consistency before exposing task metadata. */
export function parseBatchTask(value: unknown, expected?: BatchEnvelope, expectedTaskID?: string): BatchTask | null {
  if (!object(value) || !hash(value.id) || !requestID(value.request_id) || !member(taskStates, value.state)
    || !sourceNames(value.sources) || typeof value.force !== "boolean" || !timestamp(value.created_at)
    || !Array.isArray(value.items) || !value.items.length || value.items.length > 50 || !object(value.progress)) return null;
  if (expectedTaskID !== undefined && (typeof expectedTaskID !== "string" || !/^[a-f\d]{32}$/i.test(expectedTaskID) || value.id !== expectedTaskID.toLowerCase())) return null;
  const task: BatchTask = { id: value.id, request_id: value.request_id, state: value.state, sources: [...value.sources], force: value.force,
    created_at: value.created_at, progress: { total: value.items.length, queued: 0, running: 0, completed: 0, failed: 0, canceled: 0 }, items: [] };
  if (value.finished_at !== undefined) {
    if (!timestamp(value.finished_at) || Date.parse(value.finished_at) < Date.parse(task.created_at)) return null;
    task.finished_at = value.finished_at;
  }
  if (value.cancel_reason !== undefined) {
    if (!member(cancelReasons, value.cancel_reason)) return null;
    task.cancel_reason = value.cancel_reason;
  }
  const hashes = new Set<string>();
  for (const raw of value.items) {
    const item = parseItem(raw, task.sources, task.created_at);
    if (!item || hashes.has(item.node_hash) || (task.finished_at && item.finished_at && Date.parse(item.finished_at) > Date.parse(task.finished_at))) return null;
    hashes.add(item.node_hash);
    task.items.push(item);
    task.progress[item.state]++;
  }
  for (const key of ["total", ...itemStates] as const) {
    const count = value.progress[key];
    if (typeof count !== "number" || !Number.isSafeInteger(count) || count < 0 || count !== task.progress[key]) return null;
  }
  if (task.progress.running > 2) return null;
  const active = task.progress.queued + task.progress.running;
  if (isTerminal(task) !== (task.finished_at !== undefined) || (isTerminal(task) && active !== 0)) return null;
  if (task.state === "queued" && (task.progress.queued !== task.progress.total || task.cancel_reason !== undefined)) return null;
  // finishLocked can leave "running" between one completion and the next
  // claim. cancelLocked, however, drains every queued item under the same lock.
  if (task.state === "running" && (active === 0 || task.progress.queued === task.progress.total || task.cancel_reason !== undefined || task.progress.canceled !== 0)) return null;
  if (task.state === "completed" && (task.cancel_reason !== undefined || task.progress.canceled !== 0)) return null;
  if (task.state === "canceling" && (!task.cancel_reason || task.progress.queued !== 0 || task.progress.running === 0)) return null;
  if (task.state === "canceled" && (!task.cancel_reason || task.progress.canceled === 0)) return null;

  if (expected !== undefined) {
    if (!object(expected) || !requestID(expected.request_id) || !Array.isArray(expected.node_hashes) || !expected.node_hashes.length || expected.node_hashes.length > 50
      || Array.from(expected.node_hashes).some(item => typeof item !== "string" || !/^[a-f\d]{32}$/i.test(item)) || !sourceNames(expected.sources) || typeof expected.force !== "boolean") return null;
    const expectedHashes = [...new Set(expected.node_hashes.map(item => item.toLowerCase()))];
    if (task.request_id !== expected.request_id || task.force !== expected.force || !sameSet([...hashes], expectedHashes) || !sameSet(task.sources, expected.sources)) return null;
  }
  return task;
}

export function isTerminal(task: BatchTask): boolean {
  return task.state === "completed" || task.state === "canceled";
}

export function batchErrorKey(status?: number, operation: "prepare" | "submit" | "poll" | "cancel" = "poll"): string {
  if (status === 400) return "任务范围、来源或标识无效，请检查后重试。";
  if (status === 401 || status === 403) return "无权访问任务，请检查管理员登录状态。";
  if (status === 429) return "任务请求过于频繁，请稍后手动重试。";
  if (operation === "prepare") return "无法读取来源，请检查插件配置后手动重试。";
  if (status === 404) return "任务记录不可获取：可能已过期、被淘汰或属于旧配置；不代表从未执行。";
  if (status === 409) return "请求标识已用于其他范围，请保留原提交或明确创建新任务。";
  if (operation === "cancel") return "取消请求未获确认；任务可能仍在运行，请查询原任务。";
  if (operation === "submit") return status === 503 ? "插件不可用、任务容量已满或请求超时；请保留原提交，重试可能再次执行。" : "未能确认任务是否接受；请保留原提交，重试可能再次执行。";
  return status === 503 ? "插件不可用或任务查询超时；已暂停查询，服务端任务状态未知。" : "任务查询失败；已暂停查询，服务端任务可能继续运行。";
}
