# PR #1 插件化实现方案与执行规范

> 状态：实施指令草案
> 关联 PR：[feat(ipcheck): 节点出口 IP 质量检测特性提案](https://github.com/58cdn/Resin/pull/1)
> 插件基线：PR #2 已合并，合并提交为 `f1624eab9dcf1c4bbf2f5b821c6df9338193db4a`

## 1. 目标

将 PR #1 的节点出口 IP 质量检测实现为 Resin 的首方内置插件 `resin.ip-quality`，而不是把检测逻辑散落在代理热路径、节点池或主程序启动流程中。

实现必须满足：

- 检测逻辑由插件生命周期管理，支持配置、启停和状态查询。
- 检测结果按出口 IP 或节点 Hash 缓存并复用。
- 支持经节点检测和按 IP 检测两类数据源。
- 提供单节点手动检测和结果查询 API。
- 检测失败不影响节点健康、熔断、延迟统计和路由选择。
- 默认关闭后台检测，不未经用户同意向第三方发送出口 IP。
- 后续可以在冷路径接入 Platform IP 质量过滤。

## 2. 实施边界

### 2.1 插件形态

首个版本使用 builtin plugin：

- 插件 ID：`resin.ip-quality`
- 领域引擎：`internal/ipcheck`
- 插件适配器：`internal/plugin/builtin/ip_quality.go`
- 插件由现有 `plugin.Manager` 管理
- API handler 由宿主通过受控接口注册到现有 API 路由

当前外部子进程插件只支持 request hook 和 event handler，不能直接访问节点池、出站连接或 `cache.db`。因此 P1 不把这些内部能力暴露给外部插件。未来如需第三方数据源插件化，应另行设计受限 RPC capability，不允许插件直接拿到数据库连接或 `GlobalNodePool` 指针。

### 2.2 不得违反的原则

1. 不在请求代理热路径执行检测、外部 HTTP 请求、数据库查询或复杂聚合。
2. 检测失败不得调用 `GlobalNodePool.RecordResult(false)`。
3. 检测结果不得改变节点健康状态、熔断状态、延迟统计或现有探测统计。
4. 核心二进制不引入 Playwright、Chromium 或其他无头浏览器。
5. 密钥只从环境变量读取，不进入 plugin config、`system_config` 或普通 API 响应。
6. 所有结果允许 `unknown`，不能用默认值伪造未知数据。
7. 所有外部请求必须设置超时、响应体大小上限和错误隔离。
8. 可配置目标只允许 HTTPS，并拒绝 localhost、回环、链路本地和私网地址，防止 SSRF。
9. 测试不得依赖公网或实时第三方页面，使用 `httptest` 和固定响应 fixture。
10. 非官方接口必须显式配置后才能启用；无法确认稳定性或使用条款时返回 `unavailable`。

## 3. 宿主扩展接口

现有插件协议不具备节点读取、经节点 HTTP、缓存和 API 注册能力。新增一个最小的、通用的 builtin host capability，不为 IP 检测写旁路服务。

建议提供以下能力，具体命名应遵循仓库现有接口风格：

```go
type IPQualityHost interface {
    NodeSnapshot(ctx context.Context, hash node.Hash) (NodeSnapshot, bool)
    DoViaNode(ctx context.Context, hash node.Hash, req *http.Request) (*http.Response, error)
    DoDirect(ctx context.Context, req *http.Request) (*http.Response, error)
    GeoIPLookup(ctx context.Context, ip netip.Addr) (GeoIPInfo, bool)
    IPCheckStore() IPCheckStore
}
```

`NodeSnapshot` 只返回只读数据：

- node hash
- egress IP
- enabled
- healthy
- platform/subscription 归属

`DoViaNode` 必须由宿主控制 transport 和目标安全策略，不能把可任意修改的 `http.Transport` 暴露给插件。

建议增加可选的 host-aware builtin 接口，保持现有 builtin 的 `New func() pluginsdk.Plugin` 行为不变。外部插件不应获得上述 host capability。

## 4. 统一数据模型

在 `internal/ipcheck` 定义 source 和结果模型：

```go
type SourceKind string

const (
    SourceKindViaNode SourceKind = "via_node"
    SourceKindByIP    SourceKind = "by_ip"
)

type CheckRequest struct {
    NodeHash node.Hash
    EgressIP netip.Addr
    HTTP     *http.Client
}

type Source interface {
    Name() string
    Kind() SourceKind
    RequiresKey() bool
    Check(context.Context, CheckRequest) (*SourceResult, error)
}
```

每个 `SourceResult` 至少包含：

- source name
- source kind
- ok
- checked_at
- expires_at
- data
- error

统一结果格式：

```json
{
  "egress_ip": "203.0.113.10",
  "checked_at": "2026-10-01T12:00:00Z",
  "summary": {
    "country": "unknown",
    "city": "unknown",
    "asn": null,
    "as_org": "unknown",
    "ip_type": "unknown",
    "ip_origin": "unknown",
    "fraud_score": null,
    "abuse_flags": [],
    "cf_challenge": "unknown",
    "egress_consistent": null,
    "egress_ips_by_site": {},
    "services": {
      "claude": "unknown",
      "chatgpt": "unknown"
    },
    "risk_level": "unknown"
  },
  "sources": []
}
```

取值约定：

- `ip_type`：`residential` / `datacenter` / `mobile` / `unknown`
- `ip_origin`：`native` / `broadcast` / `unknown`
- `cf_challenge`：`none` / `challenge` / `blocked` / `unknown`
- `services.*`：`available` / `region_blocked` / `risky` / `unknown`
- `risk_level`：`low` / `medium` / `high` / `unknown`

聚合规则：

- 完整保留原始 source result。
- `fraud_score` 默认取可用源中的最大值。
- `ip_type` 和 `ip_origin` 按可配置的源优先级取第一个非 `unknown` 值。
- `abuse_flags` 取各源并集。
- `summary` 是可重新计算的派生视图，不能作为唯一事实来源。

## 5. 持久化、缓存和失效

复用 `cache.db` 的迁移、批量刷盘和关闭流程，增加：

```sql
ip_check_results(
    subject_kind TEXT NOT NULL,
    subject TEXT NOT NULL,
    source TEXT NOT NULL,
    result_json TEXT NOT NULL,
    checked_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL,
    PRIMARY KEY(subject_kind, subject, source)
);
```

规则：

- by-ip 结果按 `egress_ip` 归属。
- via-node 结果按 `node_hash` 归属。
- 同一 `egress_ip + source` 的并发 by-ip 查询使用 singleflight 去重。
- 同一 subject/source 的 in-flight 请求必须合并。
- `force=true` 跳过有效缓存，但仍然合并 in-flight 请求。
- 节点出口 IP 变化时失效该节点的 via-node 结果。
- 节点物理删除时清理 node 结果。
- IP 结果按 TTL 自然过期。
- 缓存读写不能阻塞代理热路径。

## 6. P1：MVP

P1 只实现单节点手动检测和查询，不实现后台调度、批量检测或 Platform 过滤。

### 6.1 无密钥数据源

#### egress-consistency

- 类型：`via_node`
- 经节点并发请求多个 trace/IP echo 目标。
- 默认至少包含现有出口探测使用的 Cloudflare trace 端点。
- 目标列表可配置，必须通过 HTTPS 和 SSRF 校验。
- 输出 `egress_ips_by_site` 和 `egress_consistent`。
- 单个目标失败只影响该 source。

#### db-ip

- 类型：`by_ip` 或离线 GeoIP provider。
- 优先复用现有 GeoIP 下载、校验、原子替换和读取框架。
- 支持 country、city、ASN、AS organization。
- 按 CC BY 4.0 要求补充数据来源说明。
- 数据库不可用时返回 `unavailable`，不填充伪造字段。

#### ippure

- 仅在接口格式和使用条款确认后启用。
- 默认可以保持显式配置源。
- 解析 `fraudScore`、`isResidential`、`isBroadcast`。
- 非 JSON、字段变化或挑战页返回 source error。

#### Cloudflare challenge probe

- 类型：`via_node`
- 识别 `cf-mitigated: challenge`。
- 识别 403/503 和以下挑战页特征：`Just a moment...`、`challenge-platform`、`cf-turnstile`。
- 输出 `none`、`challenge`、`blocked` 或 `unknown`。

#### AI region probe

- 类型：`via_node`
- 支持 Claude 和 ChatGPT 的地区/可用性判断。
- 不执行登录，不发送用户数据。
- 输出 `available`、`region_blocked`、`risky` 或 `unknown`。
- 支持可配置的支持地区列表。
- 测试不能依赖实时官方页面。

### 6.2 P1 API

沿用现有鉴权、中间件和错误格式：

- `POST /api/v1/nodes/{hash}/actions/check-ip`
- `GET /api/v1/nodes/{hash}/ip-check`
- `GET /api/v1/ip-checks/{ip}`
- `POST /api/v1/ip-checks/lookup`
- `GET /api/v1/ip-checks/sources`

单节点请求体：

```json
{
  "sources": ["egress-consistency", "db-ip"],
  "force": false
}
```

任意 IP 查询体：

```json
{
  "ip": "203.0.113.10",
  "sources": ["db-ip"],
  "force": false
}
```

要求：

- 节点不存在返回 404。
- IP 或请求体非法返回 400。
- 节点没有有效 egress IP 时返回明确错误。
- source 不可用时返回 source 状态，不伪造成功结果。
- 不允许 by-ip API 调用 via-node source。
- 不返回 secret、运行目录、数据库路径或完整内部配置。

### 6.3 P1 配置

```json
{
  "enabled_sources": [
    "egress-consistency",
    "db-ip"
  ],
  "ip_result_ttl": "24h",
  "node_result_ttl": "6h",
  "request_timeout_ms": 10000,
  "consistency_targets": [
    "https://cloudflare.com/cdn-cgi/trace"
  ],
  "background_enabled": false
}
```

要求：

- `background_enabled` 默认 false。
- 配置严格拒绝未知字段。
- TTL、timeout、目标数量和 URL 长度有上限。
- 未配置 key 的 source 自动禁用并提供原因。
- key 只从环境变量读取：
  - `RESIN_IP_CHECK_SCAMALYTICS_USER`
  - `RESIN_IP_CHECK_SCAMALYTICS_KEY`
  - `RESIN_IP_CHECK_IPDATA_KEY`
  - `RESIN_IP_CHECK_CLOUDFLARE_RADAR_TOKEN`

### 6.4 P1 测试

必须覆盖：

- 每个 source 的 fixture 解析。
- 非法配置和未知字段。
- consistency 多目标成功、部分失败和超时。
- Cloudflare challenge 分类。
- AI region 分类。
- db-ip 查询和数据库不可用。
- 缓存命中、过期和 force。
- by-ip 按出口 IP 去重。
- via-node 按 node hash 隔离。
- singleflight 并发合并。
- source 失败不影响其他 source。
- 出口 IP 变化导致 via-node 结果失效。
- 检测失败不调用 `RecordResult(false)`。
- API 400、404、not checked 和 source unavailable。
- API 不泄露 key 和内部路径。
- 插件启停、配置更新和 Manager Stop。
- 所有网络测试使用 `httptest`，不访问公网。

## 7. P2：展示和后台能力

P1 全部通过后，单独提交 P2：

- 节点详情 IP 质量面板。
- 节点列表紧凑 IP 质量徽标。
- by-ip 查询界面。
- 批量检测 API 和进度查询。
- 显式开启的后台周期检测。
- 每个 source 独立 QPS、日配额和源级熔断。
- Scamalytics、ipdata、Cloudflare Radar 等需 key 数据源。
- source 状态、配额和错误原因展示。
- 可测试、可配置、公开规则的 risk_level 聚合。
- 中英文 i18n。

后台检测仍必须默认关闭。

## 8. P3：Platform 冷路径过滤

P2 全部通过后，单独提交 P3：

- `Platform.ip_quality_filters`。
- `ip_types`、`max_fraud_score`、`exclude_abuse_flags`。
- `require_services`、`require_egress_consistent`。
- `unknown_action` 默认 `allow`。
- 结果变化只触发 Platform dirty/rebuild。
- 过滤器变化触发冷路径全量重建。
- 节点列表增加对应过滤参数。
- 热路径不执行网络请求、数据库查询或复杂聚合。
- 同步更新 `DESIGN.md` 的 Platform 路由过滤说明。

## 9. 暂不实现项目

以下内容保持为后续调研或独立提案：

- DNS leak 检测。
- 浏览器自动化和 Playwright。
- 没有稳定公开 API 的检测站点直接接入。
- 未确认条款的 HTML 抓取源。
- 外部子进程插件直接访问节点池或 cache.db。
- 将 IP 质量检测结果混入健康探测、熔断或代理热路径。

## 10. 文档要求

实现阶段同步更新：

- `DESIGN.md`
- `doc/plugins.md`
- `doc/plugins.zh-CN.md`
- IP 质量检测使用说明

文档必须说明：

- 插件启用方式。
- 数据源来源、许可和稳定性边界。
- TTL、缓存归属和 unknown 语义。
- 第三方结果仅供参考。
- key 的环境变量配置。
- 后台检测默认关闭。
- DNS/browser 检测暂不支持的原因。

## 11. 阶段验收

每个阶段单独提交，先通过当前阶段验收再进入下一阶段。

Go 验证：

```powershell
$env:PATH = "$HOME\sdk\go\bin;$env:PATH"
go test ./internal/ipcheck ./internal/plugin ./internal/api ./internal/service
go test ./...
go build ./...
go vet ./...
```

WebUI 阶段验证：

```powershell
$env:PATH = "/d/Apps/nvm/.nodejs;$env:PATH"
npm run build
npx eslint <modified-plugin-files>
```

交付前检查：

- `git diff --check` 通过。
- 没有提交 `webui/dist`。
- 测试和 fixture 不包含真实 API key、用户数据或公网依赖。
- 使用 Conventional Commit。
- 对未实现的 P2、P3、P4 项逐项说明原因。
- 不得在只完成 P1 时宣称 PR #1 全部实现。

## 12. 建议提交顺序

1. `feat(ipcheck): add plugin host and P1 detection engine`
2. `feat(ipcheck): add manual APIs and persistence`
3. `feat(ipcheck): add quality dashboard and background checks`
4. `feat(ipcheck): add platform quality filters`
5. `docs(ipcheck): document sources, privacy and limitations`

如果 P1 的 host capability 或缓存模型评审未通过，应停止后续阶段，先修正接口，不要提前实现 WebUI 或 Platform 过滤。
