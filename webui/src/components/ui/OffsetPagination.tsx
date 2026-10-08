import { useMemo, useState } from "react";
import { Button } from "./Button";
import { Input } from "./Input";
import { Select } from "./Select";
import { useI18n } from "../../i18n";

// A select with one option per page gets slow to render and hard to use on large lists
// (a million-node pool has thousands of pages), so beyond this the jump becomes a number input.
const MAX_PAGE_SELECT_OPTIONS = 100;

type OffsetPaginationProps = {
  page: number;
  totalPages: number;
  totalItems: number;
  pageSize: number;
  pageSizeOptions: readonly number[];
  disabled?: boolean;
  onPageChange: (page: number) => void;
  onPageSizeChange: (pageSize: number) => void;
};

export function OffsetPagination({
  page,
  totalPages,
  totalItems,
  pageSize,
  pageSizeOptions,
  disabled = false,
  onPageChange,
  onPageSizeChange,
}: OffsetPaginationProps) {
  const { t } = useI18n();
  // Page number being typed into the jump input; null shows the current page.
  const [pageInput, setPageInput] = useState<string | null>(null);
  const normalizedTotalPages = Math.max(1, totalPages);
  const normalizedPage = Math.min(Math.max(page, 0), normalizedTotalPages - 1);
  const pageStart = totalItems === 0 ? 0 : normalizedPage * pageSize + 1;
  const pageEnd = Math.min((normalizedPage + 1) * pageSize, totalItems);
  const showPageSelect = normalizedTotalPages <= MAX_PAGE_SELECT_OPTIONS;

  const pageOptions = useMemo(() => {
    if (normalizedTotalPages > MAX_PAGE_SELECT_OPTIONS) {
      return [];
    }
    return Array.from({ length: normalizedTotalPages }, (_, index) => index);
  }, [normalizedTotalPages]);

  const jumpTo = (nextPage: number) => {
    const bounded = Math.min(Math.max(nextPage, 0), normalizedTotalPages - 1);
    onPageChange(bounded);
  };

  const commitPageInput = () => {
    if (pageInput === null) {
      return;
    }
    setPageInput(null);
    const target = Number.parseInt(pageInput, 10);
    if (Number.isFinite(target)) {
      jumpTo(target - 1);
    }
  };

  return (
    <div className="nodes-pagination">
      <p className="nodes-pagination-meta">
        {t("第 {{page}} / {{pages}} 页 · 显示 {{start}}-{{end}} / {{total}}", {
          page: normalizedPage + 1,
          pages: normalizedTotalPages,
          start: pageStart,
          end: pageEnd,
          total: totalItems,
        })}
      </p>
      <div className="nodes-pagination-controls">
        <label className="nodes-page-size">
          <span>{t("每页")}</span>
          <Select disabled={disabled} value={String(pageSize)} onChange={(event) => onPageSizeChange(Number(event.target.value))}>
            {pageSizeOptions.map((size) => (
              <option key={size} value={size}>
                {size}
              </option>
            ))}
          </Select>
        </label>

        <label className="nodes-page-jump">
          <span>{t("跳至")}</span>
          {showPageSelect ? (
            <Select
              value={String(normalizedPage)}
              onChange={(event) => jumpTo(Number(event.target.value))}
              aria-label={t("选择页码")}
              disabled={disabled}
            >
              {pageOptions.map((index) => (
                <option key={index} value={index}>
                  {index + 1}
                </option>
              ))}
            </Select>
          ) : (
            <Input
              type="number"
              min={1}
              max={normalizedTotalPages}
              value={pageInput ?? String(normalizedPage + 1)}
              onChange={(event) => setPageInput(event.target.value)}
              onBlur={commitPageInput}
              onKeyDown={(event) => {
                if (event.key === "Enter") {
                  event.preventDefault();
                  commitPageInput();
                } else if (event.key === "Escape") {
                  setPageInput(null);
                }
              }}
              aria-label={t("输入页码")}
              disabled={disabled}
            />
          )}
          <span>{t("页")}</span>
        </label>

        <Button variant="secondary" size="sm" onClick={() => jumpTo(normalizedPage - 1)} disabled={disabled || normalizedPage <= 0}>
          {t("上一页")}
        </Button>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => jumpTo(normalizedPage + 1)}
          disabled={disabled || normalizedPage >= normalizedTotalPages - 1}
        >
          {t("下一页")}
        </Button>
      </div>
    </div>
  );
}
