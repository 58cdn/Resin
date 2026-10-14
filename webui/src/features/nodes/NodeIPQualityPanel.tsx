import { useQuery } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { useI18n } from "../../i18n";
import { ApiError } from "../../lib/api-client";
import { formatDateTime } from "../../lib/time";
import { getNodeIPQuality, listIPQualitySources } from "./ip-quality-api";
import { cacheMatchesNode, isFreshSource, nextExpiryDelay, qualityValueLabel } from "./ip-quality";
import "./ip-quality.css";

type Props = { nodeHash: string; egressIP: string; egressVersion: string; active: boolean };

function cacheErrorKey(error: Error): string {
  if (error instanceof ApiError) {
    if (error.status === 404) return "暂无有效缓存，可能尚未检测、缓存已过期或节点已移除。";
    if (error.status === 409) return "节点出口不可用或已变化，请刷新节点详情。";
    if (error.status === 503) return "IP 质量插件未启用或暂不可用。";
    if (error.status === 401 || error.status === 403) return "无权读取 IP 质量结果，请检查管理员登录状态。";
  }
  return "读取 IP 质量缓存失败，请重试。";
}

export function NodeIPQualityPanel({ nodeHash, egressIP, egressVersion, active }: Props) {
  const { t } = useI18n();
  const resultQuery = useQuery({
    queryKey: ["node-ip-quality", nodeHash, egressIP, egressVersion],
    queryFn: ({ signal }) => getNodeIPQuality(nodeHash, signal),
    enabled: active && Boolean(nodeHash),
    retry: false,
    gcTime: 0,
    refetchOnWindowFocus: false,
  });
  const sourcesQuery = useQuery({
    queryKey: ["ip-quality-sources"],
    queryFn: ({ signal }) => listIPQualitySources(signal),
    enabled: active,
    retry: false,
    gcTime: 0,
    refetchOnWindowFocus: false,
  });
  const [clock, setClock] = useState(Date.now);
  const now = Math.max(clock, resultQuery.dataUpdatedAt);
  const pluginUnavailable = sourcesQuery.error instanceof ApiError && sourcesQuery.error.status === 503;
  const result = resultQuery.isError || pluginUnavailable ? undefined : resultQuery.data;

  // Expiry changes presentation only: never schedule a probe or polling request.
  useEffect(() => {
    if (!active || !result) return;
    const delay = nextExpiryDelay(result.sources, now, Date.now());
    if (delay === null) return;
    const timer = window.setTimeout(() => setClock(Date.now()), delay);
    return () => window.clearTimeout(timer);
  }, [active, result, now]);

  const mismatched = result && !cacheMatchesNode(result, egressIP);
  const expired = result?.sources.some((source) => !isFreshSource(source, now));
  const summary = result && !mismatched && !expired && result.sources.length ? result.summary : undefined;
  const text = (value: string | null | undefined) => value && value !== "unknown" ? value : t("未知");
  const numeric = (value: number | null | undefined) => typeof value === "number" && Number.isFinite(value) ? String(value) : t("未知");
  const busy = resultQuery.isFetching || sourcesQuery.isFetching;
  const fields = summary ? [
    [t("国家 / 城市"), `${text(summary.country)} / ${text(summary.city)}`],
    ["ASN", numeric(summary.asn)],
    [t("网络组织"), text(summary.as_org)],
    [t("IP 类型 / 来源"), `${t(qualityValueLabel(summary.ip_type))} / ${t(qualityValueLabel(summary.ip_origin))}`],
    [t("出口一致性"), summary.egress_consistent === true ? t("一致") : summary.egress_consistent === false ? t("不一致") : t("未知")],
    [t("Cloudflare 挑战"), t(qualityValueLabel(summary.cf_challenge))],
    ["ChatGPT", t(qualityValueLabel(summary.services?.chatgpt))],
    ["Claude", t(qualityValueLabel(summary.services?.claude))],
    [t("欺诈分数 / 风险等级"), `${numeric(summary.fraud_score)} / ${t(qualityValueLabel(summary.risk_level))}`],
    [t("滥用标记"), summary.abuse_flags?.length ? summary.abuse_flags.join(", ") : t("暂无标记，不代表无风险")],
  ] : [];

  return (
    <section className="platform-drawer-section ip-quality-panel" aria-label={t("IP 质量缓存")}>
      <div className="ip-quality-heading">
        <div className="platform-drawer-section-head">
          <h4>{t("IP 质量缓存")}</h4>
          <p>{t("仅读取已有结果；打开和刷新不会触发检测。")}</p>
        </div>
        <Button size="sm" variant="secondary" disabled={!active || busy} onClick={() => { void resultQuery.refetch(); void sourcesQuery.refetch(); }}>
          <RefreshCw size={14} aria-hidden="true" />{t("刷新缓存")}
        </Button>
      </div>
      <div aria-live="polite" aria-busy={busy}>
        {resultQuery.isPending ? <p className="muted">{t("正在读取 IP 质量缓存...")}</p> : null}
        {resultQuery.error ? <p className="callout callout-warning">{t(cacheErrorKey(resultQuery.error))}</p> : null}
        {!resultQuery.error && pluginUnavailable ? <p className="callout callout-warning">{t("IP 质量插件未启用或暂不可用。")}</p> : null}
        {mismatched ? <p className="callout callout-warning">{t("缓存出口与当前节点不一致，已隐藏旧结果。请刷新节点详情。")}</p> : null}
        {!mismatched && expired ? <p className="callout callout-warning">{t("部分缓存已过期，已隐藏摘要。请刷新缓存。")}</p> : null}
        {result && !mismatched ? <p className="ip-quality-caption">{t("观测出口")}: {result.egress_ip} · {t("最近观测")}: {formatDateTime(result.checked_at)}</p> : null}
        {summary ? <dl className="ip-quality-facts">{fields.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl> : null}
      </div>
      {result && !mismatched && result.sources.length > 0 ? <div className="ip-quality-observations">
        <h5>{t("来源观测")}</h5>
        {result.sources.map((source) => {
          const fresh = isFreshSource(source, now);
          const label = !fresh ? "已过期或时间未知" : source.status === "unavailable" ? "不可用" : source.ok && source.status === "ok" ? "已获取观测" : "检测失败";
          return <details key={source.source} className="ip-quality-source">
            <summary><span>{source.source}</span><Badge variant={label === "已获取观测" ? "info" : "muted"}>{t(label)}</Badge></summary>
            <p>{t(source.kind === "via_node" ? "经节点观测" : "按 IP 查询")} · {t("观测时间")}: {formatDateTime(source.checked_at)}</p>
            <p>{t("到期时间")}: {formatDateTime(source.expires_at)}</p>
            {source.error ? <p className="ip-quality-source-error">{t("来源错误")}: {source.error}</p> : null}
            {fresh ? <pre>{JSON.stringify(source.data, null, 2)}</pre> : null}
          </details>;
        })}
      </div> : null}
      <details className="ip-quality-catalog">
        <summary>{t("数据源状态")}</summary>
        {sourcesQuery.isPending ? <p>{t("正在读取数据源状态...")}</p> : null}
        {sourcesQuery.error ? <p>{t("无法读取数据源状态。请检查插件配置或重试。")}</p> : null}
        {!sourcesQuery.error ? sourcesQuery.data?.map((source) => <div key={source.name} className="ip-quality-catalog-row">
          <div><strong>{source.name}</strong><Badge variant="muted">{t(!source.available ? "不可用" : source.enabled ? "已启用" : "未启用")}</Badge></div>
          {source.reason ? <p>{source.reason}</p> : null}
          {source.attribution ? <p>{source.attribution}</p> : null}
        </div>) : null}
      </details>
      <p className="ip-quality-caption">{t("结果是历史观测，不代表节点健康或账户可用性；未知不等于安全。")}</p>
      <Link className="ip-quality-settings" to="/plugins">{t("查看插件配置")}</Link>
    </section>
  );
}
