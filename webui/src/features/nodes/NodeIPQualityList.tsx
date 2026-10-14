import { useRef } from "react";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { useI18n } from "../../i18n";
import { qualityValueLabel } from "./ip-quality";
import type { QualityCacheEntry } from "./ip-quality-batch";
import type { useNodeIPQualityBatch } from "./use-node-ip-quality-batch";
import "./ip-quality.css";

type Batch = ReturnType<typeof useNodeIPQualityBatch>;

export function NodeIPQualityListToolbar({ batch }: { batch: Batch }) {
  const { t } = useI18n();
  const actions = useRef<HTMLDivElement>(null);
  const busy = batch.phase === "reading";
  const progress = t("已读取 {{completed}} / {{total}} 个节点的缓存", { completed: batch.completed, total: batch.total });
  return <div className="ip-quality-list-toolbar">
    <div className="ip-quality-list-actions" ref={actions}>
      <Button size="sm" variant="secondary" disabled={!batch.enabled || !batch.total || busy} onClick={() => void batch.start()}>{t("读取本页质量缓存")}</Button>
      <Button size="sm" variant="ghost" disabled={!busy} onClick={() => { batch.cancel(); requestAnimationFrame(() => actions.current?.querySelector("button")?.focus()); }}>{t("取消读取")}</Button>
      {busy ? <progress value={batch.completed} max={batch.total} aria-label={t("本页缓存读取进度")} /> : null}
    </div>
    <p className="ip-quality-caption">{t("仅读取已有结果，不会触发检测。徽标不代表节点健康或安全。")}</p>
    <p className="ip-quality-list-status" role="status" aria-live="polite" aria-atomic="true">
      {batch.phase === "idle" ? t("尚未读取本页质量缓存。") : batch.phase === "canceled" ? t("已取消，保留已完成的缓存读取。") + " " + progress : batch.phase === "error" ? t(batch.errorKey) + " " + progress : progress}
    </p>
  </div>;
}

export function NodeIPQualityCacheBadge({ entry, now, busy, name, onOpen }: { entry?: QualityCacheEntry; now: number; busy: boolean; name: string; onOpen: () => void }) {
  const { t } = useI18n();
  const kind = entry?.expiresAt !== null && entry?.expiresAt !== undefined && entry.expiresAt <= now ? "expired" : entry?.kind;
  const labels = { cached: "有缓存", partial: "部分来源失败", sources_failed: "来源未成功", empty: "暂无缓存", conflict: "出口不可用或已变化", too_large: "结果较大，查看详情", failed: "读取失败", mismatch: "出口已变化", expired: "缓存已过期" };
  const label = kind ? t(labels[kind]) : t(busy ? "等待读取" : "未读取");
  const hasEvidence = kind === "cached" || kind === "partial" || kind === "sources_failed";
  return <button type="button" className="ip-quality-cache-button" aria-label={t("查看 {{name}} 的 IP 质量缓存：{{status}}", { name, status: label })} onClick={(event) => { event.stopPropagation(); onOpen(); }}>
    <Badge variant={kind === "cached" ? "info" : kind === "partial" || kind === "sources_failed" ? "warning" : "muted"}>{label}</Badge>
    {hasEvidence && entry ? <small>{t(qualityValueLabel(entry.ipType))} · {t("{{count}} 个来源", { count: entry.sourceCount })}</small> : null}
  </button>;
}
