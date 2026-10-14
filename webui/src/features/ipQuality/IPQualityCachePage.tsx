import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { ArrowLeft, Search } from "lucide-react";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { useI18n } from "../../i18n";
import { formatDateTime } from "../../lib/time";
import { isFreshSource, nextExpiryDelay, qualityValueLabel } from "../nodes/ip-quality";
import { ipCacheStatus, normalizeCacheIP } from "./cache";
import { useIPCache } from "./use-ip-cache";
import "../nodes/ip-quality.css";
import "./ip-cache.css";

const statusKeys = {
  cached: "已读取有效缓存", partial: "部分来源观测失败", empty: "暂无有效缓存",
  expired: "缓存已过期，已隐藏摘要。可手动重新读取。", unavailable: "已保存的来源观测不可用", failed: "来源观测均未成功",
};

export function IPQualityCachePage() {
  const { t } = useI18n();
  const [input, setInput] = useState("");
  const [invalid, setInvalid] = useState(false);
  const formRef = useRef<HTMLFormElement>(null);
  const focusInput = () => formRef.current?.querySelector("input")?.focus();
  const cache = useIPCache();
  const { state } = cache;
  const busy = state.phase === "loading";
  const result = state.result;
  const [clock, setClock] = useState(Date.now);
  const now = Math.max(clock, state.receivedAt ?? 0);
  const status = result ? ipCacheStatus(result, now) : undefined;
  const summary = status === "cached" || status === "partial" ? result?.summary : undefined;
  useEffect(() => {
    if (!result) return;
    const delay = nextExpiryDelay(result.sources, now, Date.now());
    if (delay === null) return;
    const timer = setTimeout(() => setClock(Date.now()), delay);
    return () => clearTimeout(timer);
  }, [result, now]);

  const text = (value: unknown) => typeof value === "string" && value && value !== "unknown" ? value : t("未知");
  const number = (value: unknown) => typeof value === "number" && Number.isFinite(value) ? String(value) : t("未知");
  const facts = summary ? [
    [t("国家 / 城市"), `${text(summary.country)} / ${text(summary.city)}`],
    ["ASN", number(summary.asn)], [t("网络组织"), text(summary.as_org)],
    [t("IP 类型 / 来源"), `${t(qualityValueLabel(summary.ip_type))} / ${t(qualityValueLabel(summary.ip_origin))}`],
    [t("欺诈分数 / 风险等级"), `${number(summary.fraud_score)} / ${t(qualityValueLabel(summary.risk_level))}`],
    [t("滥用标记"), Array.isArray(summary.abuse_flags) && summary.abuse_flags.length ? summary.abuse_flags.filter(flag => typeof flag === "string").join(", ") : t("暂无标记，不代表无风险")],
  ] : [];
  const message = state.phase === "idle" ? "输入公网 IP，读取 Resin 已保存的按 IP 观测。"
    : state.phase === "loading" ? "正在读取 IP 质量缓存..."
    : state.phase === "canceled" ? "已取消读取，结果已清除。"
    : state.error ?? (status ? statusKeys[status] : "暂无有效缓存");

  return <section className="ip-cache-page">
    <header className="module-header">
      <div>
        <Link className="ip-cache-back" to="/resources"><ArrowLeft size={16} aria-hidden="true" />{t("返回资源")}</Link>
        <h2>{t("按 IP 查看质量缓存")}</h2>
        <p className="module-description">{t("仅读取已有按 IP 结果，不发起检测，也不包含经节点的可用性观测。")}</p>
      </div>
    </header>
    <Card className="platform-directory-card ip-cache-card">
      <form ref={formRef} aria-label={t("IP 缓存查询")} noValidate onSubmit={event => {
        event.preventDefault();
        if (!normalizeCacheIP(input)) { cache.reset(); setInvalid(true); focusInput(); return; }
        setInvalid(false); void cache.read(input);
      }}>
        <label htmlFor="ip-cache-address">{t("公网 IP 地址")}</label>
        <div className="ip-cache-controls">
          <Input id="ip-cache-address" value={input} autoComplete="off" autoCapitalize="none" spellCheck={false} maxLength={64}
            aria-invalid={invalid} invalid={invalid} aria-describedby={invalid ? "ip-cache-help ip-cache-input-error" : "ip-cache-help"}
            onChange={event => { cache.reset(); setInput(event.target.value); setInvalid(false); }}
            onBlur={() => { if (input.trim()) setInvalid(!normalizeCacheIP(input)); }} />
          <Button type="submit" disabled={busy}><Search size={16} aria-hidden="true" />{t("读取缓存")}</Button>
          <Button variant="secondary" disabled={!busy} onClick={() => { cache.cancel(); focusInput(); }}>{t("取消读取")}</Button>
        </div>
        <p id="ip-cache-help" className="ip-cache-help">{t("支持 IPv4 和 IPv6。输入地址后点击读取；没有自动检测或刷新。")}</p>
        {invalid ? <p id="ip-cache-input-error" className="ip-cache-input-error" role="alert">{t("请输入 IPv4 或 IPv6 地址，不支持域名、网址、端口、CIDR 或区域标识。")}</p> : null}
      </form>
      <div className="ip-cache-status" role="status" aria-label={t("IP 缓存读取状态")} aria-live="polite" aria-atomic="true">
        {state.ip ? <p className="ip-cache-address">{state.ip}</p> : null}
        <p>{t(message)}</p>
        {state.sourcesError ? <p>{t("无法读取数据源状态。请检查插件配置或重试。")}</p> : null}
      </div>
      {result ? <section className="ip-cache-result" aria-label={t("按 IP 缓存结果")}>
        <p className="ip-cache-help">{t("最近观测")}: {formatDateTime(result.checked_at)}</p>
        {summary ? <dl className="ip-quality-facts">{facts.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl> : null}
        {result.sources.length ? <div className="ip-quality-observations">
          <h3>{t("来源观测")}</h3>
          {result.sources.map(source => {
            const fresh = isFreshSource(source, now);
            const label = !fresh ? "已过期或时间未知" : source.status === "unavailable" ? "不可用" : source.ok && source.status === "ok" ? "已获取观测" : "检测失败";
            return <details className="ip-quality-source" key={source.source}>
              <summary><span>{source.source}</span><Badge variant={label === "已获取观测" ? "info" : "muted"}>{t(label)}</Badge></summary>
              <p>{t("观测时间")}: {formatDateTime(source.checked_at)}</p>
              <p>{t("到期时间")}: {formatDateTime(source.expires_at)}</p>
              {source.error ? <p className="ip-quality-source-error">{t("来源错误")}: {source.error}</p> : null}
              {fresh ? <pre>{JSON.stringify(source.data, null, 2)}</pre> : null}
            </details>;
          })}
        </div> : null}
        <details className="ip-quality-catalog">
          <summary>{t("按 IP 数据源状态")}</summary>
          <p>{t("来源已启用或可用，不代表本次观测成功或离线库已加载。")}</p>
          {state.sources?.map(source => <div className="ip-quality-catalog-row" key={source.name}>
            <div><strong>{source.name}</strong><Badge variant="muted">{t(!source.available ? "不可用" : source.enabled ? "已启用" : "未启用")}</Badge></div>
            {source.reason ? <p>{source.reason}</p> : null}{source.attribution ? <p>{source.attribution}</p> : null}
          </div>)}
        </details>
      </section> : null}
      <p className="ip-cache-help">{t("结果是历史观测，不代表节点健康或账户可用性；未知不等于安全。")}</p>
      <Link className="ip-cache-back" to="/plugins">{t("查看插件配置")}</Link>
    </Card>
  </section>;
}
