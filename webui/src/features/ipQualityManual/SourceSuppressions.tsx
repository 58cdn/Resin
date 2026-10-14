import { useI18n } from "../../i18n";
import { formatDateTime } from "../../lib/time";
import { suppressionLabels } from "./suppression";
import type { SuppressedSource } from "./suppression";

export function SourceSuppressions({ sources }: { sources?: SuppressedSource[] }) {
  const { t } = useI18n();
  if (!sources?.length) return null;
  return <section aria-label={t("本地来源请求限制")}>
    <h4>{t("本地来源请求限制")}</h4>
    <p className="ip-manual-note">{t("这些来源未返回完整观测，旧缓存未被此次限制覆盖。部分请求可能已执行；限制不表示节点故障或质量结论。")}</p>
    {sources.map(source => <div className="ip-quality-source" key={source.source}>
      <strong>{source.source}</strong>
      <p>{t("已准入 {{admitted}} 次，已限制 {{suppressed}} 次", { admitted: source.admitted, suppressed: source.suppressed })}</p>
      <ul>{source.reasons.map(reason => <li key={reason.reason}>{t(suppressionLabels[reason.reason])} ({reason.count}){reason.retry_at ? <> · {t("最早重新评估时间")}: {formatDateTime(reason.retry_at)}</> : null}</li>)}</ul>
    </div>)}
    <p className="ip-manual-note">{t("最早时间不保证届时准入；本页不会自动重试检测，force 也不能绕过来源限制。")}</p>
  </section>;
}
