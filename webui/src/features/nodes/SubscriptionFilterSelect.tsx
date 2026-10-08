import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown } from "lucide-react";
import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type MouseEvent,
  type UIEvent,
} from "react";
import { Button } from "../../components/ui/Button";
import { Input } from "../../components/ui/Input";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { getSubscription, listSubscriptions } from "../subscriptions/api";
import type { Subscription } from "../subscriptions/types";

// Options are fetched a page at a time: opening the list loads the first page, and
// scrolling to the bottom (or "Load more") adds the next one. Pages are cached one by
// one and a reopened list starts again from the first page, so only the pages being
// shown are fetched or refetched.
const OPTIONS_PAGE_SIZE = 50;
const KEYWORD_DEBOUNCE_MS = 300;
const LOAD_MORE_SCROLL_THRESHOLD_PX = 48;
const LOAD_MORE_KEYBOARD_THRESHOLD = 5;

const POPOVER_STYLE: CSSProperties = {
  position: "absolute",
  top: "calc(100% + 4px)",
  left: 0,
  zIndex: 30,
  width: "max(100%, 260px)",
  maxWidth: "min(360px, 90vw)",
  display: "flex",
  flexDirection: "column",
  border: "1px solid var(--border)",
  borderRadius: "12px",
  background: "var(--surface-strong)",
  boxShadow: "var(--shadow)",
  overflow: "hidden",
};
const SEARCH_INPUT_STYLE: CSSProperties = {
  padding: "4px 8px",
  fontSize: "0.875rem",
  minHeight: "32px",
  height: "32px",
};
const LIST_STYLE: CSSProperties = {
  position: "relative",
  maxHeight: "280px",
  overflowY: "auto",
  padding: "4px",
};
const OPTION_STYLE: CSSProperties = {
  padding: "6px 10px",
  borderRadius: "8px",
  fontSize: "0.875rem",
  cursor: "pointer",
  whiteSpace: "nowrap",
  overflow: "hidden",
  textOverflow: "ellipsis",
};
const STATUS_STYLE: CSSProperties = {
  padding: "6px 10px",
  fontSize: "0.8125rem",
  color: "var(--text-muted)",
};
const FOOTER_STYLE: CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  gap: "8px",
  padding: "4px 6px 4px 10px",
  borderTop: "1px solid var(--border)",
  fontSize: "0.75rem",
  color: "var(--text-muted)",
};
const COMPACT_BUTTON_STYLE: CSSProperties = {
  padding: "2px 8px",
  flexShrink: 0,
};

type SubscriptionOption = {
  id: string;
  label: string;
  subscription?: Subscription;
};

type SubscriptionFilterSelectProps = {
  id?: string;
  value: string;
  onChange: (subscriptionId: string) => void;
  style?: CSSProperties;
};

function keepSearchFocus(event: MouseEvent<HTMLElement>) {
  event.preventDefault();
}

export function SubscriptionFilterSelect({ id, value, onChange, style }: SubscriptionFilterSelectProps) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const listboxId = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [keywordInput, setKeywordInput] = useState("");
  const [keyword, setKeyword] = useState("");
  const [pageCount, setPageCount] = useState(1);
  const [activeIndex, setActiveIndex] = useState(0);

  const resetList = useCallback(() => {
    setOpen(false);
    setKeywordInput("");
    setKeyword("");
    setPageCount(1);
  }, []);

  useEffect(() => {
    const next = keywordInput.trim();
    if (next === keyword) {
      return;
    }
    const timeoutID = window.setTimeout(() => {
      setKeyword(next);
      setPageCount(1);
      setActiveIndex(0);
    }, KEYWORD_DEBOUNCE_MS);
    return () => window.clearTimeout(timeoutID);
  }, [keywordInput, keyword]);

  useEffect(() => {
    if (!open) {
      return;
    }
    const handlePointerDown = (event: PointerEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        resetList();
      }
    };
    document.addEventListener("pointerdown", handlePointerDown);
    return () => document.removeEventListener("pointerdown", handlePointerDown);
  }, [open, resetList]);

  const pageQueries = useQueries({
    queries: Array.from({ length: pageCount }, (_, pageIndex) => ({
      queryKey: ["subscriptions", "node-filter-options", keyword, pageIndex],
      queryFn: ({ signal }) =>
        listSubscriptions(
          {
            limit: OPTIONS_PAGE_SIZE,
            offset: pageIndex * OPTIONS_PAGE_SIZE,
            keyword,
          },
          signal
        ),
      enabled: open,
      staleTime: 60_000,
    })),
  });

  const loadedSubscriptions: Subscription[] = [];
  const loadedIDs = new Set<string>();
  let total = 0;
  for (const pageQuery of pageQueries) {
    if (!pageQuery.data) {
      continue;
    }
    total = pageQuery.data.total;
    for (const subscription of pageQuery.data.items) {
      // Offsets shift when subscriptions are added between page loads.
      if (!loadedIDs.has(subscription.id)) {
        loadedIDs.add(subscription.id);
        loadedSubscriptions.push(subscription);
      }
    }
  }
  const firstPageQuery = pageQueries[0];
  const lastPageQuery = pageQueries[pageQueries.length - 1];
  const failedPageQuery = pageQueries.find((pageQuery) => pageQuery.isError);
  const hasNextPage = lastPageQuery.data !== undefined && pageCount * OPTIONS_PAGE_SIZE < lastPageQuery.data.total;
  const isLoadingMore = pageCount > 1 && lastPageQuery.isPending;

  const options: SubscriptionOption[] = loadedSubscriptions.map((subscription) => ({
    id: subscription.id,
    label: subscription.name,
    subscription,
  }));
  if (!keyword) {
    options.unshift({ id: "", label: t("全部") });
  }
  const activeOptionIndex = Math.min(activeIndex, options.length - 1);

  const selectedFromOptions = value ? loadedSubscriptions.find((subscription) => subscription.id === value) : undefined;
  const selectedSubscriptionQuery = useQuery({
    queryKey: ["subscriptions", "detail", value],
    queryFn: ({ signal }) => getSubscription(value, signal),
    enabled: value !== "" && !selectedFromOptions,
    staleTime: 60_000,
    retry: false,
  });
  const selectedLabel = value ? (selectedFromOptions?.name ?? selectedSubscriptionQuery.data?.name ?? value) : t("全部");

  const closeList = (restoreFocus: boolean) => {
    resetList();
    if (restoreFocus) {
      triggerRef.current?.focus();
    }
  };

  const scrollOptionIntoView = (index: number) => {
    const list = listRef.current;
    const option = list?.querySelector<HTMLElement>(`[data-index="${index}"]`);
    if (!list || !option) {
      return;
    }
    if (option.offsetTop < list.scrollTop) {
      list.scrollTop = option.offsetTop;
    } else if (option.offsetTop + option.offsetHeight > list.scrollTop + list.clientHeight) {
      list.scrollTop = option.offsetTop + option.offsetHeight - list.clientHeight;
    }
  };

  const openList = () => {
    const initialIndex = Math.max(
      options.findIndex((option) => option.id === value),
      0
    );
    setActiveIndex(initialIndex);
    setOpen(true);
    window.requestAnimationFrame(() => scrollOptionIntoView(initialIndex));
  };

  const loadMore = () => {
    if (hasNextPage) {
      // An absolute count keeps a burst of scroll events from skipping pages.
      setPageCount(pageCount + 1);
    }
  };

  const selectOption = (option: SubscriptionOption) => {
    if (option.subscription) {
      // The trigger keeps its label without a lookup after the list resets to its first page.
      queryClient.setQueryData<Subscription>(["subscriptions", "detail", option.id], option.subscription);
    }
    closeList(true);
    if (option.id !== value) {
      onChange(option.id);
    }
  };

  const moveActiveOption = (delta: number) => {
    if (options.length === 0) {
      return;
    }
    const next = Math.min(Math.max(activeOptionIndex + delta, 0), options.length - 1);
    setActiveIndex(next);
    scrollOptionIntoView(next);
    if (next >= options.length - LOAD_MORE_KEYBOARD_THRESHOLD) {
      loadMore();
    }
  };

  const handleTriggerKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (!open && (event.key === "ArrowDown" || event.key === "ArrowUp")) {
      event.preventDefault();
      openList();
    }
  };

  const handleSearchKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    switch (event.key) {
      case "ArrowDown":
      case "ArrowUp":
        event.preventDefault();
        moveActiveOption(event.key === "ArrowDown" ? 1 : -1);
        break;
      case "Enter": {
        event.preventDefault();
        const pendingKeyword = keywordInput.trim();
        if (pendingKeyword !== keyword) {
          // Search right away rather than pick from the previous keyword's results.
          setKeyword(pendingKeyword);
          setPageCount(1);
          setActiveIndex(0);
        } else if (activeOptionIndex >= 0) {
          selectOption(options[activeOptionIndex]);
        }
        break;
      }
      case "Escape":
        event.preventDefault();
        closeList(true);
        break;
      case "Tab":
        closeList(false);
        break;
    }
  };

  const handleListScroll = (event: UIEvent<HTMLDivElement>) => {
    const list = event.currentTarget;
    if (list.scrollHeight - list.scrollTop - list.clientHeight <= LOAD_MORE_SCROLL_THRESHOLD_PX) {
      loadMore();
    }
  };

  return (
    <div ref={rootRef} style={{ position: "relative", width: "100%" }}>
      <button
        ref={triggerRef}
        type="button"
        id={id}
        className="select"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listboxId : undefined}
        title={selectedLabel}
        onClick={() => {
          if (open) {
            closeList(false);
          } else {
            openList();
          }
        }}
        onKeyDown={handleTriggerKeyDown}
        style={{
          ...style,
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          gap: "4px",
          textAlign: "left",
          cursor: "pointer",
        }}
      >
        <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{selectedLabel}</span>
        <ChevronDown size={14} aria-hidden="true" style={{ flexShrink: 0, color: "var(--text-muted)" }} />
      </button>

      {open ? (
        <div style={POPOVER_STYLE}>
          <div style={{ padding: "6px" }}>
            <Input
              autoFocus
              role="combobox"
              aria-expanded="true"
              aria-controls={listboxId}
              aria-autocomplete="list"
              aria-activedescendant={activeOptionIndex >= 0 ? `${listboxId}-${activeOptionIndex}` : undefined}
              aria-label={t("搜索订阅")}
              placeholder={t("搜索订阅")}
              value={keywordInput}
              onChange={(event) => setKeywordInput(event.target.value)}
              onKeyDown={handleSearchKeyDown}
              style={SEARCH_INPUT_STYLE}
            />
          </div>

          <div
            ref={listRef}
            id={listboxId}
            role="listbox"
            aria-label={t("来自此订阅")}
            onScroll={handleListScroll}
            style={LIST_STYLE}
          >
            {options.map((option, index) => {
              const selected = option.id === value;
              return (
                <div
                  key={option.id || "__all__"}
                  id={`${listboxId}-${index}`}
                  role="option"
                  aria-selected={selected}
                  data-index={index}
                  title={option.label}
                  onMouseDown={keepSearchFocus}
                  onMouseEnter={() => setActiveIndex(index)}
                  onClick={() => selectOption(option)}
                  style={{
                    ...OPTION_STYLE,
                    background: index === activeOptionIndex ? "var(--primary-soft)" : undefined,
                    color: selected ? "var(--primary)" : undefined,
                    fontWeight: selected ? 600 : undefined,
                  }}
                >
                  {option.label}
                </div>
              );
            })}

            {firstPageQuery.isPending ? <div style={STATUS_STYLE}>{t("正在加载订阅数据...")}</div> : null}

            {keyword && firstPageQuery.isSuccess && loadedSubscriptions.length === 0 ? (
              <div style={STATUS_STYLE}>{t("没有匹配的订阅")}</div>
            ) : null}

            {failedPageQuery ? (
              <div
                style={{
                  ...STATUS_STYLE,
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                  gap: "8px",
                  color: "var(--danger)",
                }}
              >
                <span>{t("加载订阅失败：{{message}}", { message: formatApiErrorMessage(failedPageQuery.error, t) })}</span>
                <Button
                  size="sm"
                  variant="ghost"
                  onMouseDown={keepSearchFocus}
                  onClick={() => void failedPageQuery.refetch()}
                  style={COMPACT_BUTTON_STYLE}
                >
                  {t("重试")}
                </Button>
              </div>
            ) : null}
          </div>

          {loadedSubscriptions.length > 0 ? (
            <div style={FOOTER_STYLE}>
              <span>{t("已加载 {{loaded}} / {{total}}", { loaded: loadedSubscriptions.length, total })}</span>
              {hasNextPage || isLoadingMore ? (
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={isLoadingMore}
                  onMouseDown={keepSearchFocus}
                  onClick={loadMore}
                  style={COMPACT_BUTTON_STYLE}
                >
                  {isLoadingMore ? t("正在加载订阅数据...") : t("加载更多")}
                </Button>
              ) : null}
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
