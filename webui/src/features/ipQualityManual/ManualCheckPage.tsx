import { ArrowLeft, Play, Square } from "lucide-react";
import { useEffect, useState } from "react";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { useI18n } from "../../i18n";
import { formatDateTime } from "../../lib/time";
import { isFreshSource, nextExpiryDelay, qualityValueLabel } from "../nodes/ip-quality";
import type { IPQualityCheckResult } from "../nodes/ip-quality";
import { SourceSuppressions } from "./SourceSuppressions";
import { normalizeSubject, resultStatus, selectable } from "./model";
import type { SubjectKind } from "./model";
import { useManualCheck } from "./use-manual-check";
import "../nodes/ip-quality.css";
import "./manual-check.css";

export function ManualCheckRoute() {
  const location = useLocation();
  return <ManualCheckPage key={location.search} />;
}

export function ManualCheckPage() {
  const { t } = useI18n();
  const [params] = useSearchParams();
  const initialNode = params.get("node") ?? "";
  const [kind, setKind] = useState<SubjectKind>(initialNode ? "node" : "ip");
  const [value, setValue] = useState(initialNode);
  const [selected, setSelected] = useState<string[]>([]);
  const [force, setForce] = useState(false);
  const [invalid, setInvalid] = useState(false);
  const focusSubject = () => document.getElementById("manual-subject")?.focus();
  const { state, prepare, execute, cancel, reset, clearResult } = useManualCheck();
  const busy = ["prepare", "check", "cache"].includes(state.phase);
  const prepared = state.prepared;
  const [clock, setClock] = useState(Date.now);
  const now = Math.max(clock, state.receivedAt ?? 0);
  useEffect(() => {
    if (!state.result) return;
    const delay = nextExpiryDelay(state.result.sources, now, Date.now());
    if (delay === null) return;
    const timer = window.setTimeout(() => setClock(Date.now()), delay);
    return () => window.clearTimeout(timer);
  }, [state.result, now]);
  function resetForm() { reset(); setSelected([]); setForce(false); setInvalid(false); }
  const message = state.message ?? (state.phase === "prepare" ? "正在读取对象和来源，不会发起检测。" : state.phase === "check" ? "正在等待单次检测；本页最多等待 130 秒。" : state.phase === "cache" ? "正在读取当前缓存，不会发起检测。" : state.result ? resultStatus(state.result, now) : prepared ? "对象已准备，请选择来源后显式执行。" : "请输入检测对象；加载对象与来源不会发起检测。");

  return <section className="ip-manual-page">
    <Link className="ip-manual-back" to={initialNode ? "/nodes" : "/resources"}><ArrowLeft size={16} aria-hidden="true" />{t(initialNode ? "返回节点" : "返回资源")}</Link>
    <header className="module-header"><div><h2>{t("手动 IP 质量检测")}</h2><p className="module-description">{t("明确选择对象和来源，只执行一次；不会自动检测、重试或后台调度。")}</p></div></header>
    <Card className="ip-manual-card">
      <form onSubmit={event => { event.preventDefault(); if (busy) return; const valid = normalizeSubject(kind, value); setInvalid(!valid); if (!valid) { focusSubject(); return; } setSelected([]); setForce(false); void prepare(kind, value); }}>
        <div className="ip-manual-inputs">
          <label htmlFor="manual-kind">{t("检测对象类型")}<select id="manual-kind" value={kind} onChange={event => { resetForm(); setKind(event.target.value as SubjectKind); setValue(""); }}><option value="ip">{t("公网 IP")}</option><option value="node">{t("单个节点")}</option></select></label>
          <label htmlFor="manual-subject">{t(kind === "ip" ? "公网 IP 地址" : "节点 Hash（32 位十六进制）")}<Input id="manual-subject" value={value} autoComplete="off" spellCheck={false} aria-invalid={invalid} aria-describedby={`manual-help${invalid ? " manual-error" : ""}`} onChange={event => { resetForm(); setValue(event.target.value); }} onBlur={() => { if (value.trim()) setInvalid(!normalizeSubject(kind, value)); }} /></label>
          <Button type="submit" variant="secondary" disabled={busy}>{t("加载对象与来源")}</Button>
        </div>
        <p id="manual-help" className="ip-manual-note">{t(kind === "ip" ? "支持 IPv4 和 IPv6；地址范围最终由服务端校验。" : "节点需已有公网出口；此操作不会探测或修改节点出口。")}</p>
        {invalid ? <p id="manual-error" role="alert" className="ip-quality-source-error">{t(kind === "ip" ? "请输入有效的 IPv4 或 IPv6 地址。" : "请输入完整的 32 位节点 Hash。")}</p> : null}
      </form>
      <div className="ip-manual-status" role="status" aria-label={t("手动检测状态")} aria-live="polite" aria-atomic="true"><strong>{t(message)}</strong></div>
      {prepared ? <>
        <div className="ip-manual-object"><h3>{t("本次对象")}</h3>{prepared.node?.name ? <p>{prepared.node.name}</p> : null}<code>{prepared.value}</code>{prepared.kind === "node" ? <p>{t("准备时出口")}: {prepared.ip}</p> : null}</div>
        <fieldset className="ip-manual-sources" disabled={busy}><legend>{t("选择本次来源")}</legend>
          {prepared.sources.filter(source => kind === "node" || source.kind === "by_ip").map(source => {
            const allowed = selectable(source, kind);
            return <div className="ip-manual-source-option" key={source.name}><label><input type="checkbox" checked={selected.includes(source.name)} disabled={!allowed} value={source.name} onChange={event => { clearResult(); setSelected(current => event.target.checked ? [...current, source.name] : current.filter(name => name !== source.name)); }} /><strong>{source.name}</strong><Badge variant="muted">{t(allowed ? "可选择" : "不可选择")}</Badge></label>
              <p>{t(source.kind === "by_ip" ? "按 IP 查询；当前 DB-IP 使用本地数据库。" : "通过所选节点请求已配置的外部目标，目标能看到节点出口 IP。")}</p>
              {source.reason ? <p>{source.reason}</p> : null}{source.requires_key ? <p>{t("本控件不使用需要密钥的来源。")}</p> : null}{source.attribution ? <p>{source.attribution}</p> : null}
            </div>;
          })}
          {!prepared.sources.some(source => selectable(source, kind)) ? <p>{t("没有可选择的来源，请检查插件配置；本页不会启用插件。")}</p> : null}
        </fieldset>
        <p className="ip-manual-note">{t("目录可用不代表数据库就绪或观测成功。具体外部目标在插件配置中维护，本页不更改目标。")}</p>
        <label className="ip-manual-force"><input type="checkbox" checked={force} disabled={busy} onChange={event => { clearResult(); setForce(event.target.checked); }} />{t("跳过有效缓存（force）")}</label>
        <p className="ip-manual-note">{t(force ? "跳过有效缓存，但仍可能合并同对象、同来源的在途检测。" : "默认允许复用有效缓存，不保证产生新的外部请求；以观测时间为准。")}</p>
        <div className="ip-manual-actions"><Button disabled={busy || !selected.length} onClick={() => void execute(selected, force)}><Play size={16} aria-hidden="true" />{t("执行一次检测")}</Button><Button variant="secondary" disabled={busy} onClick={() => void execute([], false, true)}>{t("读取当前缓存核对")}</Button></div>
      </> : null}
      <Button className="ip-manual-stop" variant="secondary" disabled={!busy} onClick={() => { cancel(); focusSubject(); }}><Square size={14} aria-hidden="true" />{t("停止等待")}</Button>
      <p className="ip-manual-note">{t("停止、超时或失败不代表服务端未执行；已完成的缓存不回滚，共享检测可能继续。")}</p>
      {state.result ? <ManualResult result={state.result} now={now} cache={state.operation === "cache"} kind={kind} /> : null}
      <Link className="ip-quality-settings" to="/plugins">{t("查看插件配置")}</Link>
      <Link className="ip-quality-settings" to="/resources/ip-quality-batch">{t("批量 IP 质量任务")}</Link>
    </Card>
  </section>;
}

function ManualResult({ result, now, cache, kind }: { result: IPQualityCheckResult; now: number; cache: boolean; kind: SubjectKind }) {
  const { t } = useI18n();
  const fresh = result.sources.some(source => source.ok && source.status === "ok") && result.sources.every(source => isFreshSource(source, now));
  const summary = result.summary;
  const text = (value: string | undefined | null) => value && value !== "unknown" ? value : t("未知");
  const numeric = (value: number | undefined | null) => typeof value === "number" && Number.isFinite(value) ? String(value) : t("未知");
  const fields = [
    [t("国家 / 城市"), `${text(summary.country)} / ${text(summary.city)}`], ["ASN", numeric(summary.asn)],
    [t("网络组织"), text(summary.as_org)], [t("IP 类型 / 来源"), `${t(qualityValueLabel(summary.ip_type))} / ${t(qualityValueLabel(summary.ip_origin))}`],
    ...(kind === "node" ? [
      [t("出口一致性"), summary.egress_consistent === true ? t("一致") : summary.egress_consistent === false ? t("不一致") : t("未知")],
      [t("Cloudflare 挑战"), t(qualityValueLabel(summary.cf_challenge))], ["ChatGPT", t(qualityValueLabel(summary.services?.chatgpt))], ["Claude", t(qualityValueLabel(summary.services?.claude))],
    ] : []),
    [t("欺诈分数 / 风险等级"), `${numeric(summary.fraud_score)} / ${t(qualityValueLabel(summary.risk_level))}`], [t("滥用标记"), summary.abuse_flags?.length ? summary.abuse_flags.join(", ") : t("暂无标记，不代表无风险")],
  ];
  return <div className="ip-manual-result"><h3>{t(cache ? "当前缓存响应" : "本次检测响应")}</h3><p>{result.egress_ip}{result.checked_at ? <> · {t("最近观测")}: {formatDateTime(result.checked_at)}</> : null}</p>
    <SourceSuppressions sources={result.suppressed_sources} />
    <p className="ip-manual-note">{t("响应不保证持久化成功；可显式读取当前缓存核对。未知不等于通过。")}</p>
    {fresh ? <dl className="ip-quality-facts">{fields.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl> : null}
    <h4>{t("来源观测")}</h4>{result.sources.map(source => {
      const valid = isFreshSource(source, now);
      const label = !valid ? "已过期或时间未知" : source.status === "unavailable" ? "不可用" : source.ok && source.status === "ok" ? "已获取观测" : "检测失败";
      return <details className="ip-quality-source" key={source.source}><summary><span>{source.source}</span><Badge variant={label === "已获取观测" ? "info" : "muted"}>{t(label)}</Badge></summary><p>{t(source.kind === "by_ip" ? "按 IP 查询" : "经节点观测")} · {t("观测时间")}: {formatDateTime(source.checked_at)}</p><p>{t("到期时间")}: {formatDateTime(source.expires_at)}</p>{source.error ? <p className="ip-quality-source-error">{t("来源错误")}: {source.error}</p> : null}{valid ? <pre>{JSON.stringify(source.data, null, 2)}</pre> : null}</details>;
    })}
  </div>;
}
