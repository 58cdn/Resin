import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertTriangle,
  Download,
  ExternalLink,
  Info,
  Package,
  RefreshCw,
  ScanSearch,
  Sparkles,
  Store,
  Trash2,
  Upload,
  X,
} from "lucide-react";
import { type ChangeEvent, type FormEvent, useEffect, useRef, useState } from "react";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { Select } from "../../components/ui/Select";
import { Switch } from "../../components/ui/Switch";
import { Textarea } from "../../components/ui/Textarea";
import { ToastContainer } from "../../components/ui/Toast";
import { useToast } from "../../hooks/useToast";
import { useI18n } from "../../i18n";
import { ApiError, type JsonValue } from "../../lib/api-client";
import { formatApiErrorMessage } from "../../lib/error-message";
import { getEnvConfig } from "../systemConfig/api";
import {
  getMarketplace,
  installMarketplacePlugin,
  listPlugins,
  rescanPlugins,
  uninstallPlugin,
  updatePlugin,
  uploadPlugin,
} from "./api";
import type {
  MarketplacePlugin,
  Plugin,
  PluginConfig,
  PluginConfigField,
  PluginPatch,
} from "./types";

type TranslateFn = (text: string, options?: Record<string, unknown>) => string;

type PluginTab = "installed" | "marketplace";

type FieldValue = string | boolean;

type PluginFormState = {
  enabled: boolean;
  priority: string;
  timeout_ms: string;
  fail_closed: boolean;
  fields: Record<string, FieldValue>;
  rawConfig: string;
};

const EMPTY_PLUGINS: Plugin[] = [];
const PRIORITY_MIN = -10000;
const PRIORITY_MAX = 10000;
const TIMEOUT_MIN_MS = 10;
const TIMEOUT_MAX_MS = 60000;
const PACKAGE_ACCEPT = ".zip,.tar.gz,.tgz,application/zip,application/gzip";
const EXTERNAL_DISABLED_HINT =
  "外部插件未启用，当前仅运行内置插件。设置环境变量 RESIN_EXTERNAL_PLUGINS_ENABLED=true 后可上传、安装插件包并使用插件市场。";

function statusPresentation(plugin: Plugin, t: TranslateFn) {
  if (!plugin.enabled) {
    return { label: t("已禁用"), variant: "neutral" as const };
  }
  switch (plugin.status) {
    case "running":
      return { label: t("运行中"), variant: "success" as const };
    case "starting":
      return { label: t("启动中"), variant: "warning" as const };
    case "error":
      return { label: t("异常"), variant: "danger" as const };
    default:
      return { label: t("未运行"), variant: "neutral" as const };
  }
}

function formatLatency(us: number): string {
  if (us <= 0) {
    return "-";
  }
  if (us < 1000) {
    return `${us}µs`;
  }
  return `${(us / 1000).toFixed(us < 10_000 ? 2 : 1)}ms`;
}

function fieldLabel(field: PluginConfigField): string {
  return field.label?.trim() || field.name;
}

function initialFieldValue(field: PluginConfigField, config: PluginConfig): FieldValue {
  const value: JsonValue | undefined = field.name in config ? config[field.name] : field.default;
  switch (field.type) {
    case "boolean":
      return value === true;
    case "string_list":
      return Array.isArray(value) ? value.map((item) => String(item)).join("\n") : "";
    case "json":
      return value === undefined ? "" : JSON.stringify(value, null, 2);
    default:
      return value === undefined || value === null ? "" : String(value);
  }
}

function pluginToForm(plugin: Plugin): PluginFormState {
  const fields: Record<string, FieldValue> = {};
  for (const field of plugin.config_fields) {
    fields[field.name] = initialFieldValue(field, plugin.config);
  }
  return {
    enabled: plugin.enabled,
    priority: String(plugin.priority),
    timeout_ms: String(plugin.timeout_ms),
    fail_closed: plugin.fail_closed,
    fields,
    rawConfig: JSON.stringify(plugin.config ?? {}, null, 2),
  };
}

function parseIntegerInRange(raw: string, min: number, max: number, message: string): number {
  const value = Number(raw.trim());
  if (raw.trim() === "" || !Number.isInteger(value) || value < min || value > max) {
    throw new Error(message);
  }
  return value;
}

function parseConfigFields(plugin: Plugin, form: PluginFormState, t: TranslateFn): PluginConfig {
  // Start from the stored config so keys the form does not know about survive.
  const config: PluginConfig = { ...plugin.config };
  for (const field of plugin.config_fields) {
    const label = fieldLabel(field);
    const raw = form.fields[field.name];
    if (field.type === "boolean") {
      config[field.name] = raw === true;
      continue;
    }
    const text = typeof raw === "string" ? raw : "";
    if (text.trim() === "") {
      if (field.required) {
        throw new Error(t("{{field}} 为必填项", { field: label }));
      }
      delete config[field.name];
      continue;
    }
    switch (field.type) {
      case "number": {
        const value = Number(text.trim());
        if (!Number.isFinite(value)) {
          throw new Error(t("{{field}} 必须是数字", { field: label }));
        }
        config[field.name] = value;
        break;
      }
      case "integer": {
        const value = Number(text.trim());
        if (!Number.isInteger(value)) {
          throw new Error(t("{{field}} 必须是整数", { field: label }));
        }
        config[field.name] = value;
        break;
      }
      case "string_list":
        config[field.name] = text
          .split("\n")
          .map((line) => line.trim())
          .filter((line) => line !== "");
        break;
      case "json":
        try {
          config[field.name] = JSON.parse(text) as JsonValue;
        } catch {
          throw new Error(t("{{field}} 不是合法的 JSON", { field: label }));
        }
        break;
      default:
        config[field.name] = text;
    }
  }
  return config;
}

function parseRawConfig(raw: string, t: TranslateFn): PluginConfig {
  if (raw.trim() === "") {
    return {};
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new Error(t("插件配置不是合法的 JSON"));
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(t("插件配置必须是 JSON 对象"));
  }
  return parsed as PluginConfig;
}

function parsePluginForm(plugin: Plugin, form: PluginFormState, t: TranslateFn): PluginPatch {
  const patch: PluginPatch = { enabled: form.enabled };
  if (plugin.capabilities.request_hook) {
    patch.priority = parseIntegerInRange(
      form.priority,
      PRIORITY_MIN,
      PRIORITY_MAX,
      t("优先级必须是 {{min}} 到 {{max}} 之间的整数", { min: PRIORITY_MIN, max: PRIORITY_MAX }),
    );
    patch.timeout_ms = parseIntegerInRange(
      form.timeout_ms,
      TIMEOUT_MIN_MS,
      TIMEOUT_MAX_MS,
      t("超时时间必须是 {{min}} 到 {{max}} 之间的整数（毫秒）", { min: TIMEOUT_MIN_MS, max: TIMEOUT_MAX_MS }),
    );
    patch.fail_closed = form.fail_closed;
  }
  patch.config =
    plugin.config_fields.length > 0 ? parseConfigFields(plugin, form, t) : parseRawConfig(form.rawConfig, t);
  return patch;
}

type ConfigFieldInputProps = {
  pluginId: string;
  field: PluginConfigField;
  value: FieldValue;
  disabled: boolean;
  onChange: (value: FieldValue) => void;
};

function ConfigFieldInput({ pluginId, field, value, disabled, onChange }: ConfigFieldInputProps) {
  const { t } = useI18n();
  const id = `plugin-${pluginId}-field-${field.name}`;
  const label = fieldLabel(field);
  const text = typeof value === "string" ? value : "";

  if (field.type === "boolean") {
    return (
      <div className="subscription-switch-item field-span-2">
        <label className="subscription-switch-label" htmlFor={id}>
          <span>{label}</span>
          {field.description ? (
            <span className="subscription-info-icon" title={field.description} tabIndex={0}>
              <Info size={13} />
            </span>
          ) : null}
        </label>
        <Switch
          id={id}
          checked={value === true}
          disabled={disabled}
          onChange={(event) => onChange(event.target.checked)}
        />
      </div>
    );
  }

  let control;
  switch (field.type) {
    case "text":
    case "json":
    case "string_list":
      control = (
        <Textarea
          id={id}
          rows={field.type === "text" ? 4 : 5}
          disabled={disabled}
          value={text}
          spellCheck={false}
          placeholder={field.type === "string_list" ? t("每行一项") : undefined}
          className={field.type === "json" ? "plugin-code-input" : undefined}
          onChange={(event) => onChange(event.target.value)}
        />
      );
      break;
    case "enum":
      control = (
        <Select id={id} disabled={disabled} value={text} onChange={(event) => onChange(event.target.value)}>
          {!field.required ? <option value="">{t("（默认）")}</option> : null}
          {(field.enum_values ?? []).map((option) => (
            <option key={option} value={option}>
              {option}
            </option>
          ))}
        </Select>
      );
      break;
    case "number":
    case "integer":
      control = (
        <Input
          id={id}
          type="number"
          step={field.type === "integer" ? 1 : "any"}
          inputMode={field.type === "integer" ? "numeric" : "decimal"}
          disabled={disabled}
          value={text}
          onChange={(event) => onChange(event.target.value)}
        />
      );
      break;
    case "secret":
      control = (
        <Input
          id={id}
          type="password"
          autoComplete="new-password"
          disabled={disabled}
          value={text}
          onChange={(event) => onChange(event.target.value)}
        />
      );
      break;
    default:
      control = (
        <Input id={id} disabled={disabled} value={text} onChange={(event) => onChange(event.target.value)} />
      );
  }

  return (
    <div className="field-group field-span-2">
      <label className="field-label" htmlFor={id}>
        {label}
        {field.required ? <span className="plugin-required-mark"> *</span> : null}
      </label>
      {control}
      {field.description ? <p className="plugin-field-hint">{field.description}</p> : null}
    </div>
  );
}

type PluginFormProps = {
  plugin: Plugin;
  pending: boolean;
  onClose: () => void;
  onSubmit: (patch: PluginPatch) => Promise<void>;
};

function PluginForm({ plugin, pending, onClose, onSubmit }: PluginFormProps) {
  const { t } = useI18n();
  const [form, setForm] = useState<PluginFormState>(() => pluginToForm(plugin));
  const [formError, setFormError] = useState("");
  const hasRequestHook = Boolean(plugin.capabilities.request_hook);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !pending) {
        onClose();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [onClose, pending]);

  const setField = (name: string, value: FieldValue) => {
    setForm((current) => ({ ...current, fields: { ...current.fields, [name]: value } }));
    setFormError("");
  };

  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    let patch: PluginPatch;
    try {
      patch = parsePluginForm(plugin, form, t);
    } catch (error) {
      setFormError(error instanceof Error ? error.message : t("未知错误"));
      return;
    }
    setFormError("");
    void onSubmit(patch).catch(() => undefined);
  };

  return (
    <form className="form-grid" onSubmit={handleSubmit}>
      <div className="subscription-switch-item field-span-2">
        <label className="subscription-switch-label" htmlFor={`plugin-${plugin.id}-enabled`}>
          {t("启用")}
        </label>
        <Switch
          id={`plugin-${plugin.id}-enabled`}
          checked={form.enabled}
          onChange={(event) => {
            setForm((current) => ({ ...current, enabled: event.target.checked }));
            setFormError("");
          }}
        />
      </div>

      {hasRequestHook ? (
        <>
          <div className="field-group">
            <label className="field-label" htmlFor={`plugin-${plugin.id}-priority`}>
              {t("优先级")}
            </label>
            <Input
              id={`plugin-${plugin.id}-priority`}
              type="number"
              step={1}
              min={PRIORITY_MIN}
              max={PRIORITY_MAX}
              value={form.priority}
              onChange={(event) => {
                setForm((current) => ({ ...current, priority: event.target.value }));
                setFormError("");
              }}
            />
            <p className="plugin-field-hint">{t("数值越大越先执行")}</p>
          </div>
          <div className="field-group">
            <label className="field-label" htmlFor={`plugin-${plugin.id}-timeout`}>
              {t("超时时间（毫秒）")}
            </label>
            <Input
              id={`plugin-${plugin.id}-timeout`}
              type="number"
              step={1}
              min={TIMEOUT_MIN_MS}
              max={TIMEOUT_MAX_MS}
              value={form.timeout_ms}
              onChange={(event) => {
                setForm((current) => ({ ...current, timeout_ms: event.target.value }));
                setFormError("");
              }}
            />
          </div>
          <div className="subscription-switch-item field-span-2">
            <label className="subscription-switch-label" htmlFor={`plugin-${plugin.id}-fail-closed`}>
              <span>{t("故障时拒绝请求")}</span>
              <span
                className="subscription-info-icon"
                title={t("开启后，插件超时或出错时请求将被拒绝（503）；关闭时跳过该插件继续处理请求。")}
                tabIndex={0}
              >
                <Info size={13} />
              </span>
            </label>
            <Switch
              id={`plugin-${plugin.id}-fail-closed`}
              checked={form.fail_closed}
              onChange={(event) => {
                setForm((current) => ({ ...current, fail_closed: event.target.checked }));
                setFormError("");
              }}
            />
          </div>
        </>
      ) : null}

      <div className="field-group field-span-2">
        <label className="field-label">{t("插件配置")}</label>
      </div>

      {plugin.config_fields.length > 0 ? (
        plugin.config_fields.map((field) => (
          <ConfigFieldInput
            key={field.name}
            pluginId={plugin.id}
            field={field}
            value={form.fields[field.name] ?? ""}
            disabled={pending}
            onChange={(value) => setField(field.name, value)}
          />
        ))
      ) : (
        <div className="field-group field-span-2">
          <Textarea
            rows={8}
            spellCheck={false}
            className="plugin-code-input"
            value={form.rawConfig}
            aria-label={t("插件配置")}
            onChange={(event) => {
              setForm((current) => ({ ...current, rawConfig: event.target.value }));
              setFormError("");
            }}
          />
          <p className="plugin-field-hint">{t("该插件未声明配置项，可直接编辑 JSON 对象。")}</p>
        </div>
      )}

      {formError ? (
        <div className="callout callout-error field-span-2" role="alert">
          <AlertTriangle size={14} />
          <span>{formError}</span>
        </div>
      ) : null}

      <div className="platform-config-actions">
        <Button type="submit" disabled={pending}>
          {pending ? t("保存中...") : t("保存配置")}
        </Button>
      </div>
    </form>
  );
}

function capabilityLabels(plugin: Plugin, t: TranslateFn): string[] {
  const labels: string[] = [];
  if (plugin.capabilities.request_hook) {
    labels.push(t("请求钩子"));
  }
  const events = plugin.capabilities.events ?? [];
  if (events.length > 0) {
    labels.push(t("事件订阅：{{events}}", { events: events.join(", ") }));
  }
  return labels;
}

type InstalledTabProps = {
  externalEnabled: boolean;
  showToast: (tone: "success" | "error", text: string) => void;
};

function InstalledTab({ externalEnabled, showToast }: InstalledTabProps) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [pendingEnabledStates, setPendingEnabledStates] = useState<Map<string, boolean>>(
    () => new Map(),
  );

  const pluginsQuery = useQuery({
    queryKey: ["plugins", "list"],
    queryFn: listPlugins,
    refetchInterval: 10_000,
  });
  const plugins = pluginsQuery.data ?? EMPTY_PLUGINS;
  const editingPlugin = editingId ? plugins.find((plugin) => plugin.id === editingId) ?? null : null;

  const invalidatePlugins = async () => {
    await queryClient.invalidateQueries({ queryKey: ["plugins"] });
  };

  const updateMutation = useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: PluginPatch }) => updatePlugin(id, patch),
    onSuccess: async (plugin) => {
      await invalidatePlugins();
      setEditingId(null);
      showToast("success", t("插件 {{name}} 已更新", { name: plugin.name }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const toggleEnabledMutation = useMutation({
    mutationFn: ({ plugin, enabled }: { plugin: Plugin; enabled: boolean }) =>
      updatePlugin(plugin.id, { enabled }),
    onSuccess: async (plugin, { enabled }) => {
      await invalidatePlugins();
      showToast(
        "success",
        enabled
          ? t("插件 {{name}} 已启用", { name: plugin.name })
          : t("插件 {{name}} 已禁用", { name: plugin.name }),
      );
    },
    onError: async (error) => {
      await invalidatePlugins();
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const uninstallMutation = useMutation({
    mutationFn: async (plugin: Plugin) => {
      await uninstallPlugin(plugin.id);
      return plugin;
    },
    onSuccess: async (plugin) => {
      if (editingId === plugin.id) {
        setEditingId(null);
      }
      await invalidatePlugins();
      showToast("success", t("插件 {{name}} 已卸载", { name: plugin.name }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const rescanMutation = useMutation({
    mutationFn: rescanPlugins,
    onSuccess: async (items) => {
      await invalidatePlugins();
      showToast("success", t("扫描完成，共 {{count}} 个插件", { count: items.length }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const uploadMutation = useMutation({
    mutationFn: uploadPlugin,
    onSuccess: async (plugin) => {
      await invalidatePlugins();
      showToast("success", t("插件 {{name}} {{version}} 已安装", { name: plugin.name, version: plugin.version }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const handleUploadChange = (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) {
      return;
    }
    uploadMutation.mutate(file);
  };

  const handleEnabledChange = async (plugin: Plugin, enabled: boolean) => {
    if (pendingEnabledStates.has(plugin.id)) {
      return;
    }
    setPendingEnabledStates((current) => new Map(current).set(plugin.id, enabled));
    try {
      await toggleEnabledMutation.mutateAsync({ plugin, enabled });
    } catch {
      // The mutation callback surfaces the API error.
    } finally {
      setPendingEnabledStates((current) => {
        const next = new Map(current);
        next.delete(plugin.id);
        return next;
      });
    }
  };

  const handleUninstall = async (plugin: Plugin) => {
    if (plugin.source !== "package") {
      return;
    }
    const confirmed = window.confirm(
      t("确认卸载插件 {{name}}？插件文件、数据目录与配置将被删除，操作不可撤销。", { name: plugin.name }),
    );
    if (!confirmed) {
      return;
    }
    await uninstallMutation.mutateAsync(plugin).catch(() => undefined);
  };

  const submitUpdate = async (patch: PluginPatch) => {
    if (!editingPlugin) {
      return;
    }
    await updateMutation.mutateAsync({ id: editingPlugin.id, patch });
  };

  const closeDrawer = () => setEditingId(null);

  return (
    <>
      <Card className="platform-list-card platform-directory-card endpoint-toolbar-card">
        <div className="list-card-header">
          <div>
            <h3>{t("已安装插件")}</h3>
            <p>{t("共 {{count}} 个插件", { count: plugins.length })}</p>
          </div>
          <div className="endpoint-toolbar-actions">
            <input
              ref={fileInputRef}
              type="file"
              accept={PACKAGE_ACCEPT}
              hidden
              onChange={handleUploadChange}
            />
            <Button
              variant="secondary"
              size="sm"
              onClick={() => fileInputRef.current?.click()}
              disabled={!externalEnabled || uploadMutation.isPending}
              title={externalEnabled ? t("上传 .zip 或 .tar.gz 插件包") : t(EXTERNAL_DISABLED_HINT)}
            >
              <Upload size={16} />
              {uploadMutation.isPending ? t("上传中...") : t("上传插件")}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => rescanMutation.mutate()}
              disabled={rescanMutation.isPending}
              title={t("重新扫描插件目录，并重试启动失败的插件")}
            >
              <ScanSearch size={16} />
              {t("重新扫描")}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void pluginsQuery.refetch()}
              disabled={pluginsQuery.isFetching}
            >
              <RefreshCw size={16} className={pluginsQuery.isFetching ? "spin" : undefined} />
              {t("刷新")}
            </Button>
          </div>
        </div>
      </Card>

      <Card className="platform-cards-container">
        {pluginsQuery.isLoading ? <p className="muted">{t("正在加载插件...")}</p> : null}

        {pluginsQuery.isError ? (
          <div className="callout callout-error">
            <AlertTriangle size={14} />
            <span>{formatApiErrorMessage(pluginsQuery.error, t)}</span>
          </div>
        ) : null}

        {!pluginsQuery.isLoading && !pluginsQuery.isError && plugins.length === 0 ? (
          <div className="empty-box">
            <Sparkles size={16} />
            <p>{t("暂无插件")}</p>
          </div>
        ) : null}

        <div className="endpoint-list">
          {plugins.map((plugin) => {
            const status = statusPresentation(plugin, t);
            const displayedEnabled = pendingEnabledStates.get(plugin.id) ?? plugin.enabled;
            const toggleLabel = displayedEnabled
              ? t("禁用插件 {{name}}", { name: plugin.name })
              : t("启用插件 {{name}}", { name: plugin.name });
            const facts = [
              { label: t("请求"), value: plugin.stats.requests },
              { label: t("拒绝"), value: plugin.stats.rejects },
              { label: t("错误"), value: plugin.stats.errors },
              { label: t("超时"), value: plugin.stats.timeouts },
              { label: t("平均耗时"), value: formatLatency(plugin.stats.avg_latency_us) },
              { label: t("事件投递"), value: plugin.stats.events_delivered },
              { label: t("事件丢弃"), value: plugin.stats.events_dropped },
            ];

            return (
              <article
                className="platform-tile endpoint-tile"
                key={plugin.id}
                onClick={() => setEditingId(plugin.id)}
              >
                <div className="platform-tile-head">
                  <div className="endpoint-tile-heading">
                    <span
                      className={`endpoint-status-dot is-${status.variant}`}
                      title={status.label}
                      role="img"
                      aria-label={status.label}
                    />
                    <p>{plugin.name}</p>
                    <div className="endpoint-tile-badges">
                      {plugin.version ? <Badge variant="muted">v{plugin.version}</Badge> : null}
                      <Badge variant={plugin.source === "builtin" ? "info" : "accent"}>
                        {plugin.source === "builtin" ? t("内置") : t("插件包")}
                      </Badge>
                      <Badge variant={status.variant}>{status.label}</Badge>
                      {plugin.capabilities.request_hook && plugin.fail_closed ? (
                        <Badge variant="warning">{t("故障时拒绝")}</Badge>
                      ) : null}
                    </div>
                  </div>

                  <span
                    className="endpoint-enabled-toggle"
                    title={toggleLabel}
                    onClick={(event) => event.stopPropagation()}
                  >
                    <Switch
                      checked={displayedEnabled}
                      disabled={pendingEnabledStates.has(plugin.id)}
                      onChange={(event) => void handleEnabledChange(plugin, event.target.checked)}
                      aria-label={toggleLabel}
                    />
                  </span>
                </div>

                {plugin.description ? <p className="plugin-description">{plugin.description}</p> : null}

                {plugin.last_error ? (
                  <div className="callout callout-error" role="alert">
                    <AlertTriangle size={14} />
                    <span>{t("最近错误：{{message}}", { message: plugin.last_error })}</span>
                  </div>
                ) : null}

                <div className="endpoint-tile-bottom">
                  <div className="platform-tile-facts endpoint-capabilities">
                    <span className="plugin-id">{plugin.id}</span>
                    {capabilityLabels(plugin, t).map((label) => (
                      <span className="endpoint-capability" key={label}>
                        <span className="endpoint-capability-indicator is-enabled" aria-hidden="true" />
                        <span>{label}</span>
                      </span>
                    ))}
                    {facts.map((fact) => (
                      <span className="endpoint-capability" key={fact.label}>
                        <span>{fact.label}</span>
                        <span className="endpoint-capability-state">{fact.value}</span>
                      </span>
                    ))}
                  </div>

                  {plugin.source === "package" ? (
                    <div className="subscriptions-row-actions" onClick={(event) => event.stopPropagation()}>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => void handleUninstall(plugin)}
                        disabled={uninstallMutation.isPending}
                        title={t("卸载插件")}
                        aria-label={t("卸载插件 {{name}}", { name: plugin.name })}
                        style={{ color: "var(--delete-btn-color, #c27070)" }}
                      >
                        <Trash2 size={14} />
                      </Button>
                    </div>
                  ) : null}
                </div>
              </article>
            );
          })}
        </div>
      </Card>

      {editingPlugin ? (
        <div
          className="drawer-overlay"
          role="dialog"
          aria-modal="true"
          aria-label={t("插件详情")}
          onClick={() => {
            if (!updateMutation.isPending) {
              closeDrawer();
            }
          }}
        >
          <Card className="drawer-panel" onClick={(event) => event.stopPropagation()}>
            <div className="drawer-header">
              <div>
                <h3>{editingPlugin.name}</h3>
                <p>
                  {editingPlugin.id}
                  {editingPlugin.version ? ` · v${editingPlugin.version}` : ""}
                </p>
              </div>
              <div className="drawer-header-actions">
                <Button
                  variant="ghost"
                  size="sm"
                  aria-label={t("关闭编辑面板")}
                  onClick={closeDrawer}
                  disabled={updateMutation.isPending}
                >
                  <X size={16} />
                </Button>
              </div>
            </div>

            <div className="platform-drawer-layout">
              <section className="platform-drawer-section">
                <div className="platform-drawer-section-head">
                  <h4>{t("插件信息")}</h4>
                  {editingPlugin.description ? <p>{editingPlugin.description}</p> : null}
                </div>
                <dl className="plugin-meta-list">
                  <dt>{t("来源")}</dt>
                  <dd>{editingPlugin.source === "builtin" ? t("内置") : t("插件包")}</dd>
                  <dt>{t("能力")}</dt>
                  <dd>{capabilityLabels(editingPlugin, t).join(" · ") || t("无")}</dd>
                  {editingPlugin.author ? (
                    <>
                      <dt>{t("作者")}</dt>
                      <dd>{editingPlugin.author}</dd>
                    </>
                  ) : null}
                  {editingPlugin.license ? (
                    <>
                      <dt>{t("许可证")}</dt>
                      <dd>{editingPlugin.license}</dd>
                    </>
                  ) : null}
                  {editingPlugin.homepage ? (
                    <>
                      <dt>{t("主页")}</dt>
                      <dd>
                        <a href={editingPlugin.homepage} target="_blank" rel="noreferrer noopener">
                          {editingPlugin.homepage} <ExternalLink size={12} />
                        </a>
                      </dd>
                    </>
                  ) : null}
                  <dt>{t("重启次数")}</dt>
                  <dd>{editingPlugin.stats.restarts}</dd>
                  <dt>{t("事件错误")}</dt>
                  <dd>{editingPlugin.stats.event_errors}</dd>
                </dl>
              </section>

              <section className="platform-drawer-section">
                <div className="platform-drawer-section-head">
                  <h4>{t("插件设置")}</h4>
                  <p>{t("保存后插件将以新配置重新加载。")}</p>
                </div>
                <PluginForm
                  key={editingPlugin.id}
                  plugin={editingPlugin}
                  pending={updateMutation.isPending}
                  onClose={closeDrawer}
                  onSubmit={submitUpdate}
                />
              </section>

              {editingPlugin.source === "package" ? (
                <section className="platform-drawer-section platform-ops-section">
                  <div className="platform-drawer-section-head">
                    <h4>{t("运维操作")}</h4>
                  </div>
                  <div className="platform-ops-list">
                    <div className="platform-op-item">
                      <div className="platform-op-copy">
                        <h5>{t("卸载插件")}</h5>
                        <p className="platform-op-hint">
                          {t("停止插件并删除插件文件、数据目录与配置，操作不可撤销。")}
                        </p>
                      </div>
                      <Button
                        variant="danger"
                        onClick={() => void handleUninstall(editingPlugin)}
                        disabled={uninstallMutation.isPending}
                      >
                        {uninstallMutation.isPending ? t("卸载中...") : t("卸载")}
                      </Button>
                    </div>
                  </div>
                </section>
              ) : null}
            </div>
          </Card>
        </div>
      ) : null}
    </>
  );
}

function marketplaceAction(entry: MarketplacePlugin, t: TranslateFn): { label: string; enabled: boolean } {
  if (!entry.installable) {
    return { label: entry.installed_version ? t("已安装") : t("不可安装"), enabled: false };
  }
  if (!entry.installed_version) {
    return { label: t("安装"), enabled: true };
  }
  if (entry.update_available) {
    return { label: t("更新到 v{{version}}", { version: entry.version }), enabled: true };
  }
  return { label: t("重新安装"), enabled: true };
}

type MarketplaceTabProps = {
  externalEnabled: boolean;
  showToast: (tone: "success" | "error", text: string) => void;
};

function MarketplaceTab({ externalEnabled, showToast }: MarketplaceTabProps) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const [installingId, setInstallingId] = useState<string | null>(null);

  const marketplaceQuery = useQuery({
    queryKey: ["plugins", "marketplace"],
    queryFn: getMarketplace,
    enabled: externalEnabled,
    staleTime: 60_000,
    retry: false,
  });
  const listing = marketplaceQuery.data;

  const installMutation = useMutation({
    mutationFn: installMarketplacePlugin,
    onMutate: (id) => setInstallingId(id),
    onSuccess: async (plugin) => {
      await queryClient.invalidateQueries({ queryKey: ["plugins"] });
      showToast("success", t("插件 {{name}} {{version}} 已安装", { name: plugin.name, version: plugin.version }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
    onSettled: () => setInstallingId(null),
  });

  const handleInstall = (entry: MarketplacePlugin) => {
    const confirmed = window.confirm(
      t("插件将以 Resin 进程的权限在本机运行。确认从 {{source}} 安装 {{name}} v{{version}}？", {
        source: entry.marketplace,
        name: entry.name,
        version: entry.version,
      }),
    );
    if (!confirmed) {
      return;
    }
    installMutation.mutate(entry.id);
  };

  if (!externalEnabled) {
    return (
      <Card className="platform-cards-container">
        <div className="callout callout-warning">
          <Info size={14} />
          <span>{t(EXTERNAL_DISABLED_HINT)}</span>
        </div>
      </Card>
    );
  }

  const disabledByServer =
    marketplaceQuery.error instanceof ApiError && marketplaceQuery.error.status === 409;

  return (
    <>
      <Card className="platform-list-card platform-directory-card endpoint-toolbar-card">
        <div className="list-card-header">
          <div>
            <h3>{t("插件市场")}</h3>
            <p>
              {listing && listing.sources.length > 0
                ? t("来自 {{count}} 个市场源，共 {{plugins}} 个插件", {
                    count: listing.sources.length,
                    plugins: listing.plugins.length,
                  })
                : t("通过 RESIN_PLUGIN_MARKETPLACE_URLS 配置市场源")}
            </p>
          </div>
          <div className="endpoint-toolbar-actions">
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void marketplaceQuery.refetch()}
              disabled={marketplaceQuery.isFetching}
            >
              <RefreshCw size={16} className={marketplaceQuery.isFetching ? "spin" : undefined} />
              {t("刷新")}
            </Button>
          </div>
        </div>
      </Card>

      <Card className="platform-cards-container">
        {marketplaceQuery.isLoading ? <p className="muted">{t("正在加载插件市场...")}</p> : null}

        {marketplaceQuery.isError ? (
          <div className={`callout ${disabledByServer ? "callout-warning" : "callout-error"}`}>
            <AlertTriangle size={14} />
            <span>{formatApiErrorMessage(marketplaceQuery.error, t)}</span>
          </div>
        ) : null}

        {listing?.errors.map((item) => (
          <div className="callout callout-error" key={item.url}>
            <AlertTriangle size={14} />
            <span>{t("市场源 {{url}} 读取失败：{{message}}", { url: item.url, message: item.error })}</span>
          </div>
        ))}

        {listing && listing.sources.length === 0 ? (
          <div className="empty-box">
            <Store size={16} />
            <p>{t("未配置插件市场。设置 RESIN_PLUGIN_MARKETPLACE_URLS 为一个或多个市场索引 URL。")}</p>
          </div>
        ) : null}

        {listing && listing.sources.length > 0 && listing.plugins.length === 0 && listing.errors.length === 0 ? (
          <div className="empty-box">
            <Sparkles size={16} />
            <p>{t("市场中暂无插件")}</p>
          </div>
        ) : null}

        <div className="endpoint-list">
          {listing?.plugins.map((entry) => {
            const action = marketplaceAction(entry, t);
            const installing = installingId === entry.id;
            return (
              <article className="platform-tile endpoint-tile is-read-only" key={entry.id}>
                <div className="platform-tile-head">
                  <div className="endpoint-tile-heading">
                    <Package size={14} aria-hidden="true" />
                    <p>{entry.name}</p>
                    <div className="endpoint-tile-badges">
                      <Badge variant="muted">v{entry.version}</Badge>
                      {entry.installed_version ? (
                        <Badge variant={entry.update_available ? "warning" : "success"}>
                          {entry.update_available
                            ? t("已安装 v{{version}}，可更新", { version: entry.installed_version })
                            : t("已安装")}
                        </Badge>
                      ) : null}
                      {(entry.tags ?? []).map((tag) => (
                        <Badge variant="neutral" key={tag}>
                          {tag}
                        </Badge>
                      ))}
                    </div>
                  </div>
                  <Button
                    size="sm"
                    variant={entry.update_available || !entry.installed_version ? "primary" : "secondary"}
                    disabled={!action.enabled || installMutation.isPending}
                    onClick={() => handleInstall(entry)}
                  >
                    <Download size={14} />
                    {installing ? t("安装中...") : action.label}
                  </Button>
                </div>

                {entry.description ? <p className="plugin-description">{entry.description}</p> : null}

                {entry.reason ? (
                  <div className="callout callout-warning">
                    <Info size={14} />
                    <span>{entry.reason}</span>
                  </div>
                ) : null}

                <div className="platform-tile-facts endpoint-capabilities">
                  <span className="plugin-id">{entry.id}</span>
                  {entry.author ? (
                    <span className="endpoint-capability">
                      <span>{t("作者")}</span>
                      <span className="endpoint-capability-state">{entry.author}</span>
                    </span>
                  ) : null}
                  {entry.license ? (
                    <span className="endpoint-capability">
                      <span>{t("许可证")}</span>
                      <span className="endpoint-capability-state">{entry.license}</span>
                    </span>
                  ) : null}
                  {entry.min_resin_version ? (
                    <span className="endpoint-capability">
                      <span>{t("最低 Resin 版本")}</span>
                      <span className="endpoint-capability-state">{entry.min_resin_version}</span>
                    </span>
                  ) : null}
                  <span className="endpoint-capability">
                    <span>{t("市场源")}</span>
                    <span className="endpoint-capability-state">{entry.marketplace}</span>
                  </span>
                  {entry.homepage ? (
                    <a
                      className="endpoint-capability"
                      href={entry.homepage}
                      target="_blank"
                      rel="noreferrer noopener"
                    >
                      <ExternalLink size={12} />
                      <span>{t("主页")}</span>
                    </a>
                  ) : null}
                </div>
              </article>
            );
          })}
        </div>
      </Card>
    </>
  );
}

export function PluginsPage() {
  const { t } = useI18n();
  const { toasts, showToast, dismissToast } = useToast();
  const [tab, setTab] = useState<PluginTab>("installed");
  const envConfigQuery = useQuery({
    queryKey: ["system-config-env", "plugins"],
    queryFn: getEnvConfig,
    staleTime: 30_000,
  });
  const externalEnabled = Boolean(envConfigQuery.data?.external_plugins_enabled);
  const tabs: { key: PluginTab; label: string }[] = [
    { key: "installed", label: t("已安装") },
    { key: "marketplace", label: t("插件市场") },
  ];

  return (
    <section className="platform-page">
      <header className="module-header">
        <div>
          <h2>{t("插件")}</h2>
          <p className="module-description">
            {t("通过插件在请求链路中执行鉴权、改写与拦截，或订阅请求与租约事件对接外部系统。")}
          </p>
        </div>
      </header>

      <ToastContainer toasts={toasts} onDismiss={dismissToast} />

      {envConfigQuery.data && !externalEnabled ? (
        <div className="callout callout-warning">
          <Info size={14} />
          <span>{t(EXTERNAL_DISABLED_HINT)}</span>
        </div>
      ) : null}

      <div className="platform-detail-tabs" role="tablist" aria-label={t("插件板块")}>
        {tabs.map((item) => {
          const selected = tab === item.key;
          return (
            <button
              key={item.key}
              type="button"
              role="tab"
              aria-selected={selected}
              className={`platform-detail-tab ${selected ? "platform-detail-tab-active" : ""}`}
              onClick={() => setTab(item.key)}
            >
              <span>{item.label}</span>
            </button>
          );
        })}
      </div>

      {tab === "installed" ? (
        <InstalledTab externalEnabled={externalEnabled} showToast={showToast} />
      ) : (
        <MarketplaceTab externalEnabled={externalEnabled} showToast={showToast} />
      )}
    </section>
  );
}
