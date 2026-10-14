import { ArrowLeft, Pause, Play, Square } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { Textarea } from "../../components/ui/Textarea";
import { useI18n } from "../../i18n";
import { formatDateTime } from "../../lib/time";
import { selectable } from "../ipQualityManual/model";
import { isTerminal, normalizeBatchHashes } from "./model";
import type { BatchItem, BatchTask } from "./model";
import { useBatchTask } from "./use-batch-task";
import "../nodes/ip-quality.css";
import "../ipQualityManual/manual-check.css";
import "./batch-task.css";

const lifecycle = { queued: "排队中", running: "执行中", canceling: "正在取消", completed: "已返回结果", failed: "执行失败", canceled: "已取消" };
const observation = { all_ok: "全部来源已返回观测", partial_failure: "部分来源失败", all_failed: "所有来源失败", unavailable: "来源不可用", no_observations: "没有来源观测" };

export function BatchTaskPage() {
  const { t } = useI18n();
  const api = useBatchTask(), { state } = api;
  const [input, setInput] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [force, setForce] = useState(false);
  const [restoreID, setRestoreID] = useState("");
  const [invalid, setInvalid] = useState(false);
  const [invalidID, setInvalidID] = useState(false);
  const busy = !!state.operation, locked = !!state.envelope || !!state.taskID;
  const task = state.task;
  const focus = (id: string) => document.getElementById(id)?.focus();
  function reset() { api.reset(); setSelected([]); setForce(false); setInvalid(false); setInvalidID(false); }
  const message = state.message ?? (state.operation === "prepare" ? "正在读取来源，不会发起检测。" : state.operation === "submit" ? "正在提交固定范围的批量任务。" : state.operation === "cancel" ? "正在请求取消；请以任务响应为准。" : state.operation === "poll" && !state.observing ? "正在读取任务状态。" : task && isTerminal(task) ? "任务已结束，请查看逐节点结果；完成不等于检测通过。" : state.observing ? "正在观察任务；每次响应后间隔两秒查询，本轮最多五分钟。" : state.taskID ? "已暂停自动查询，可手动读取原任务。" : state.hashes ? "范围已准备，请选择来源后显式提交。" : "输入明确的节点列表；打开页面不会创建或查询任务。");
  return <section className="ip-manual-page ip-batch-page">
    <Link className="ip-manual-back" to="/nodes"><ArrowLeft size={16} aria-hidden="true" />{t("返回节点")}</Link>
    <header className="module-header"><div><h2>{t("批量 IP 质量任务")}</h2><p className="module-description">{t("明确提交最多 50 个节点，查看任务进度与逐节点结果。")}</p></div></header>
    <Card className="ip-manual-card">
      {!state.taskID || state.envelope ? <form onSubmit={event => { event.preventDefault(); if (busy || locked) return; if (!normalizeBatchHashes(input)) { setInvalid(true); focus("batch-hashes"); return; } setInvalid(false); setSelected([]); setForce(false); void api.prepare(input); }}>
        <label className="ip-batch-label" htmlFor="batch-hashes">{t("本次节点 Hash（每行一个）")}</label>
        <Textarea id="batch-hashes" rows={5} value={input} disabled={busy || locked} autoComplete="off" spellCheck={false} aria-invalid={invalid} aria-describedby={`batch-scope-help${invalid ? " batch-scope-error" : ""}`} onChange={event => { reset(); setInput(event.target.value); }} />
        <p id="batch-scope-help" className="ip-manual-note">{t("最多 50 个非空行，每个为 32 位十六进制；重复项会去重。不会展开筛选器、平台或订阅。")}</p>
        {invalid ? <p id="batch-scope-error" role="alert" className="ip-quality-source-error">{t("请填写 1 至 50 个有效节点 Hash；整份列表校验通过后才读取来源。")}</p> : null}
        <Button type="submit" variant="secondary" disabled={busy || locked}>{t("准备范围与来源")}</Button>
      </form> : null}
      <div className="ip-manual-status" role="status" aria-label={t("批量任务状态")} aria-live="polite" aria-atomic="true"><strong>{t(message)}</strong></div>
      {state.hashes && state.sources ? <>
        <details className="ip-batch-scope"><summary>{t("已准备 {{count}} 个节点", { count: state.hashes.length })}</summary><ul>{state.hashes.map(hash => <li key={hash}><code>{hash}</code></li>)}</ul></details>
        <p className="ip-manual-note">{t("准备只读取来源目录，不检查节点存在性或冻结出口；执行时由服务端逐节点确认，缺失或变化会单独报告。")}</p>
        <fieldset className="ip-manual-sources" disabled={busy || locked}><legend>{t("选择本次来源")}</legend>
          {state.sources.map(source => {
            const allowed = selectable(source, "node");
            return <div className="ip-manual-source-option" key={source.name}><label><input type="checkbox" value={source.name} checked={selected.includes(source.name)} disabled={!allowed} onChange={event => setSelected(names => event.target.checked ? [...names, source.name] : names.filter(name => name !== source.name))} /><strong>{source.name}</strong><Badge variant="muted">{t(allowed ? "可选择" : "不可选择")}</Badge></label><p>{t(source.kind === "by_ip" ? "按 IP 查询；当前 DB-IP 使用本地数据库。" : "通过所选节点请求已配置的外部目标，目标能看到节点出口 IP。")}</p>{source.reason ? <p>{source.reason}</p> : null}{source.requires_key ? <p>{t("本控件不使用需要密钥的来源。")}</p> : null}{source.attribution ? <p>{source.attribution}</p> : null}</div>;
          })}
          {!state.sources.some(source => selectable(source, "node")) ? <p>{t("没有可选择的来源，请检查插件配置；本页不会启用插件。")}</p> : null}
        </fieldset>
        <label className="ip-manual-force"><input type="checkbox" checked={force} disabled={busy || locked} onChange={event => setForce(event.target.checked)} />{t("跳过有效缓存（force）")}</label>
        <p className="ip-manual-note">{t(force ? "跳过有效缓存，但仍可能合并同对象、同来源的在途检测。" : "默认允许复用有效缓存，不保证产生新的外部请求；以观测时间为准。")}</p>
        <Button className="ip-batch-submit" disabled={busy || locked || !selected.length} onClick={() => void api.submit(selected, force)}><Play size={16} aria-hidden="true" />{t("提交本次批量任务")}</Button>
      </> : null}
      {state.envelope ? <div className="ip-batch-envelope"><p>{t("本次请求标识")}: <code>{state.envelope.request_id}</code></p>{!state.taskID ? <><p className="ip-manual-note">{t("未取得任务 ID 时，重试会保留完全相同的范围、来源、force 和请求标识。服务端记录可能已丢失，重试仍可能再次执行。")}</p><Button variant="secondary" disabled={busy} onClick={() => void api.retry()}>{t("手动重试原提交（可能再次执行）")}</Button></> : null}</div> : null}
      {state.taskID ? <div className="ip-batch-task-controls">
        <label className="ip-batch-label" htmlFor="batch-current-id">{t("当前任务 ID（可复制恢复）")}</label><Input id="batch-current-id" readOnly value={state.taskID} onFocus={event => event.currentTarget.select()} />
        <div className="ip-manual-actions"><Button variant="secondary" disabled={busy} onClick={() => void api.refresh()}>{t("读取一次原任务")}</Button><Button variant="secondary" disabled={busy || state.observing || !!task && isTerminal(task)} onClick={() => void api.observe()}>{t("继续观察五分钟")}</Button><Button variant="secondary" disabled={state.operation === "cancel" || state.operation === "submit" || !!task && (isTerminal(task) || task.state === "canceling")} onClick={() => void api.cancel()}><Square size={14} aria-hidden="true" />{t("请求服务端取消")}</Button></div>
      </div> : null}
      <div className="ip-manual-actions"><Button variant="secondary" disabled={!busy && !state.observing} onClick={() => api.pause()}><Pause size={14} aria-hidden="true" />{t("暂停页面观察")}</Button><Button variant="ghost" onClick={() => { reset(); requestAnimationFrame(() => focus("batch-hashes")); }}>{t("清空本页，准备新批次")}</Button></div>
      <p className="ip-manual-note">{t("离开、暂停或超时只停止页面等待。服务端取消会停止尚未开始的项；已完成缓存不回滚，在途或共享检测可能继续。")}</p>
      {task ? <TaskResult task={task} receivedAt={state.receivedAt} /> : null}
    </Card>
    <Card className="ip-manual-card ip-batch-restore"><h3>{t("按任务 ID 恢复只读查询")}</h3>
      <form onSubmit={event => { event.preventDefault(); if (busy) return; if (!/^[a-f\d]{32}$/i.test(restoreID.trim())) { setInvalidID(true); focus("batch-restore-id"); return; } setInvalidID(false); setInput(""); setSelected([]); setForce(false); void api.restore(restoreID); }}>
        <label className="ip-batch-label" htmlFor="batch-restore-id">{t("任务 ID（32 位十六进制）")}</label><Input id="batch-restore-id" value={restoreID} disabled={busy} autoComplete="off" spellCheck={false} aria-invalid={invalidID} aria-describedby={`batch-restore-help${invalidID ? " batch-id-error" : ""}`} onChange={event => { setRestoreID(event.target.value); setInvalidID(false); }} />
        {invalidID ? <p id="batch-id-error" role="alert">{t("请输入完整的 32 位任务 ID。")}</p> : null}<Button type="submit" variant="secondary" disabled={busy}>{t("恢复查询（不会重新提交）")}</Button>
      </form>
      <p id="batch-restore-help" className="ip-manual-note">{t("任务仅存于当前插件内存；重启、关闭、重新配置、容量淘汰或保留期到期后可能无法恢复。404 不代表任务从未执行。")}</p>
      <div className="ip-manual-actions"><Link className="ip-quality-settings" to="/resources/ip-quality-check">{t("手动 IP 质量检测")}</Link><Link className="ip-quality-settings" to="/plugins">{t("查看插件配置")}</Link></div>
    </Card>
  </section>;
}

function TaskResult({ task, receivedAt }: { task: BatchTask; receivedAt?: number }) {
  const { t } = useI18n();
  return <section className="ip-manual-result ip-batch-result" aria-label={t("任务历史元数据")}>
    <div className="ip-batch-result-heading"><h3>{t("任务历史元数据")}</h3><Badge variant="info">{t(lifecycle[task.state])}</Badge></div>
    <p className="ip-manual-note">{t("这是该任务返回的历史元数据，不代表当前节点出口、缓存新鲜度或风险结论；全部来源成功也不代表节点健康或账户可用。")}</p>
    <p>{t("来源")}: {task.sources.join(", ")} · force: {String(task.force)}</p><p>{t("创建时间")}: {formatDateTime(task.created_at)} · {t("本页读取时间")}: {receivedAt ? formatDateTime(new Date(receivedAt).toISOString()) : "—"}</p>
    {task.finished_at ? <p>{t("结束时间")}: {formatDateTime(task.finished_at)}</p> : null}
    {task.cancel_reason ? <p>{t("取消原因")}: {t(task.cancel_reason === "deadline" ? "任务达到服务端时限" : task.cancel_reason === "engine_stopped" ? "插件已停止或重新配置" : "已收到取消请求")}</p> : null}
    <dl className="ip-batch-progress">{(["queued", "running", "completed", "failed", "canceled"] as const).map(key => <div key={key}><dt>{t(lifecycle[key])}</dt><dd>{task.progress[key]}</dd></div>)}</dl>
    <ol className="ip-batch-items">{task.items.map(item => <ItemResult item={item} key={item.node_hash} />)}</ol>
  </section>;
}
function ItemResult({ item }: { item: BatchItem }) {
  const { t } = useI18n();
  return <li><details className="ip-quality-source"><summary><code>{item.node_hash}</code><Badge variant="muted">{t(lifecycle[item.state])}</Badge>{item.observation_status ? <span>{t(observation[item.observation_status])}</span> : null}</summary>
    {item.started_at ? <p>{t("开始时间")}: {formatDateTime(item.started_at)}</p> : null}{item.finished_at ? <p>{t("结束时间")}: {formatDateTime(item.finished_at)}</p> : null}
    {item.egress_ip ? <p>{t("任务记录的出口 IP")}: {item.egress_ip}</p> : null}{item.checked_at ? <p>{t("观测时间")}: {formatDateTime(item.checked_at)}</p> : null}
    {item.error ? <p className="ip-quality-source-error">{item.error.code}: {item.error.message}</p> : null}
    {item.sources?.map(source => <div className="ip-batch-source" key={source.source}><strong>{source.source}</strong><span>{t(source.ok ? "已获取观测" : source.status === "unavailable" ? "不可用" : "检测失败")}</span><p>{t("观测时间")}: {formatDateTime(source.checked_at)} · {t("到期时间")}: {formatDateTime(source.expires_at)}</p>{source.error ? <p className="ip-quality-source-error">{source.error}</p> : null}</div>)}
    <Link className="ip-quality-settings" to={`/resources/ip-quality-check?node=${item.node_hash}`}>{t("打开单节点页面核对当前缓存")}</Link>
  </details></li>;
}
