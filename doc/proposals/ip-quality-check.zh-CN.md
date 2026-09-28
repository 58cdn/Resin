# 特性提案：节点出口 IP 质量检测（IP Check）

* 状态：提案（Draft），待讨论
* 类型：feat
* 影响范围：新增 `internal/ipcheck` 模块、WebAPI、WebUI、`cache.db`；后续阶段涉及 Platform 过滤
* 本 PR 只包含提案文档，不包含实现代码。实现将在提案确认后分阶段提交。

---

## 1. 背景与动机

Resin 目前对每个节点只掌握两类“身份”信息：

1. **出口 IP**：由出口探测经节点请求 `https://cloudflare.com/cdn-cgi/trace` 获得（见 DESIGN.md「主动探测 / 出口探测」）。
2. **国家/地区**：由内置 GeoIP 服务查 `country.mmdb` 获得，用于 Platform 的 `RegionFilters`。

对于 Resin 面向的多账号运营场景，“能连通 + 国家对”远远不够。上层业务（尤其是 Claude、ChatGPT 等 AI 服务，以及风控严格的站点）真正关心的是：

* 这个 IP 是**住宅**还是**机房**？是**原生**还是**广播**？
* 欺诈得分/纯净度如何？是否被标记为代理、VPN、Tor、已知滥用者？
* 访问 Cloudflare 保护的站点会不会**被盾**（弹出人机挑战）？
* Claude / ChatGPT 在该出口下是否**可用**、是否处于支持地区？
* 该节点访问不同网站时，出口 IP 是否**一致**（节点侧是否做了分流）？
* 节点侧 DNS 解析是否与出口地区一致？

目前用户只能把出口 IP 复制到各个第三方网站逐一手动查询，无法覆盖成百上千乃至 100k 规模的节点，更无法把结果用于路由决策。

此外，**出口一致性**与 Resin 的核心能力直接相关：粘性路由的“同出口 IP 保持”与“同 IP 轮换”都隐含一个前提——一个节点只有一个出口 IP。如果某个节点对不同目标站点使用不同出口（机场侧分流），Resin 看到的 `EgressIP` 与业务站点实际看到的 IP 并不相同，粘性语义会被悄悄破坏。检测并标记这类节点本身就有独立价值。

## 2. 目标与非目标

### 目标

* 对节点出口 IP 提供**可插拔、多数据源**的质量检测，并把各数据源结果**归一化**为统一模型。
* **按出口 IP 去重与缓存**：大量节点共享少量出口 IP 时，只查询一次，节省第三方配额。
* 通过 WebAPI 与 WebUI 查看检测结果；支持**手动触发**与**可选的后台周期检测**。
* 为后续“按 IP 质量过滤 Platform 可路由节点”打好数据基础（冷路径实现）。

### 非目标

* **不在请求热路径上做任何检测或外部调用**。符合 DESIGN.md「热路径目标为无锁与低延迟」原则。
* **核心二进制不引入无头浏览器**（Playwright / Chromium 等）。这违背「单一二进制与零外部依赖」原则。
* **不做反爬对抗**：不破解 Cloudflare 挑战 / Turnstile；遇到挑战页即视为该数据源不可用。
* 不做浏览器侧检测（WebRTC 泄露、时区、Canvas/WebGL 指纹等）。这些是客户端环境问题，不是节点属性。
* 不承诺结果的权威性。IP 质量本质是多个第三方启发式信号的聚合，仅供参考。

## 3. 检测维度与参考数据源

检测按“发起方式”分为两类：

* **经节点自查（via-node）**：请求必须从节点出口发出，结果反映“目标站点眼中的这个节点”。分流、被盾、AI 服务可用性只能这样测。
* **按 IP 查询（by-ip）**：只需要 IP 地址即可查询第三方库或 API，可从 Resin 主机直连发起，也可经任一健康节点发起。

| # | 维度 | 说明 | 参考 | 方式 | 需要 Key | 计划阶段 |
|---|------|------|------|------|----------|----------|
| 1 | 出口一致性 / 网站分流 | 经节点访问多个 trace / IP 回显端点，比较各站点看到的出口 IP | [ip.net.coffee](https://ip.net.coffee/)、[ipsuper.com](https://ipsuper.com/) | via-node | 否 | P1 |
| 2 | 地理位置 + ASN | 城市级位置、ASN 号与 AS 组织名 | [db-ip.com](https://db-ip.com/) | by-ip（离线库） | 否 | P1 |
| 3 | IP 属性 + 纯净度 | 住宅/机房、原生/广播、纯净度系数 | [ippure.com](https://ippure.com)、[iplark.com](https://iplark.com)、ping0（参考实现） | via-node | 否 | P1 |
| 4 | 跳盾检测 | 访问 Cloudflare 保护站点是否触发挑战/拦截 | [radar.cloudflare.com](https://radar.cloudflare.com/)、[cleanip.io](https://cleanip.io/)、[iptrait.com](https://www.iptrait.com/) | via-node（+ by-ip 情报） | 部分 | P1（实测）/ P2（情报） |
| 5 | AI 服务可用性（Claude / ChatGPT） | 所在地区是否受支持、服务端点是否返回地区封锁 | [ip.net.coffee/claude](https://ip.net.coffee/claude/)、[ip.net.coffee/gpt](https://ip.net.coffee/gpt/) | via-node | 否 | P1（地区）/ P2（综合风险） |
| 6 | 欺诈得分 | 0–100 分的欺诈风险评分 | [scamalytics.com](https://scamalytics.com/) | by-ip（官方 API） | 是 | P2 |
| 7 | 滥用 / 威胁标记 | proxy / tor / datacenter / anonymous / known_abuser / known_attacker / blocklists 等 | [ipdata.co](https://ipdata.co/) | by-ip（官方 API） | 是 | P2 |
| 8 | 综合 IP 信息 | 多源交叉比对 | [ip.net.coffee/ip](https://ip.net.coffee/ip/) | —（方法论参考） | — | — |
| 9 | DNS 泄露 | 节点侧递归解析器 IP 及其地区是否与出口一致 | [ip.net.coffee/dns](https://ip.net.coffee/dns/) | via-node + 权威 DNS | — | P4（调研） |

### 3.1 各维度的实现思路

**出口一致性 / 网站分流（P1）**

* 经节点并发请求一组可配置的目标，默认候选：
  * `https://cloudflare.com/cdn-cgi/trace`（与现有出口探测一致，作为基准）
  * `https://chatgpt.com/cdn-cgi/trace`、`https://claude.ai/cdn-cgi/trace`（AI 服务侧视角，同时拿到 `loc`）
  * 一个非 Cloudflare 的 IP 回显服务（避免所有目标都在同一 CDN 上）
* 输出：`egress_ips_by_site` 映射表与布尔值 `egress_consistent`。
* 实施时需逐一验证各端点可用性，并允许用户覆盖目标列表。

**地理位置 + ASN（P1）**

* 首选 db-ip 的免费 Lite 离线库（IP to City Lite / IP to ASN Lite，mmdb 格式，CC BY 4.0，按月更新）。
* 复用现有 GeoIP 服务的“下载 → 校验 → 原子 Rename → Cron 更新 → 失败时经节点重试”框架，不引入在线查询依赖，也没有配额问题。
* 需在 WebUI / 文档中按 CC BY 4.0 要求标注数据来源。

**IP 属性 + 纯净度（P1）**

* 参考实现 [clash-ip-checker/ippure.py](https://github.com/tombcato/clash-ip-checker/blob/main/core/sources/ippure.py) 经代理请求 ippure 后端的 JSON 接口，解析 `fraudScore`、`isResidential`、`isBroadcast`。这是“查询自身出口”型接口，天然适合 via-node。
* 参考实现 [ping0.py](https://github.com/tombcato/clash-ip-checker/blob/main/core/sources/ping0.py) 用正则解析 ping0.cc 的 HTML（IP 类型、原生/广播、风险值、共享人数），遇到 Cloudflare 挑战页即降级。HTML 解析脆弱，只作为**可选、默认关闭**的源。
* 参考实现 [browser.py](https://github.com/tombcato/clash-ip-checker/blob/main/core/sources/browser.py) 用 Playwright 渲染 ippure.com 页面再抽取文本。**不进入核心**（见非目标），如确有需要，见 P4 的外部插件方案。
* 上述接口均非官方公开 API，实施前需确认稳定性与使用条款；iplark 待调研是否提供接口。

**跳盾检测（P1 实测 / P2 情报）**

* 实测：经节点请求一个或多个受 Cloudflare 保护的页面，依据以下特征判定：
  * 响应头 `cf-mitigated: challenge`（Cloudflare 官方用于标识挑战响应的头）；
  * 403/503 且响应体包含 `Just a moment...`、`challenge-platform`、`cf-turnstile` 等挑战页特征。
* 输出三态：`none` / `challenge` / `blocked`，外加 `unknown`。
* 情报补充：Cloudflare Radar API（需 API Token）提供 IP / ASN 维度的实体信息与流量情报；cleanip.io、iptrait.com 待调研是否有可程序化接入的接口。

**AI 服务可用性（P1 地区 / P2 综合风险）**

* 地区判定：经节点读取 `claude.ai`、`chatgpt.com` 的 trace `loc`，对照官方支持地区列表（[Anthropic](https://www.anthropic.com/supported-countries)、[OpenAI](https://platform.openai.com/docs/supported-countries)）。列表随官方更新，需内置默认值并允许配置覆盖。
* 端点判定：请求服务端点，识别地区封锁类的特征响应（具体特征实施时验证后固化到测试用例中）。
* 综合风险（P2）：结合 IP 属性、欺诈分、滥用标记、跳盾结果给出 `low / medium / high` 等级。评分规则公开、可配置，不做黑盒打分。

**DNS 泄露（P4，需调研）**

* 在 Resin 场景下，多数协议（vmess / vless / trojan / ss 等）把目标域名交给节点远端解析，因此有意义的信号是：**节点侧递归解析器所在地区是否与出口一致**。部分风控会关联这一点。
* 标准做法是经节点请求随机子域名，由自建权威 DNS 记录来访解析器 IP。这需要额外的基础设施，与「零外部依赖」原则存在张力。目前参考站点没有公开 API，暂列为调研项。

## 4. 总体设计

### 4.1 模块划分

新增包 `internal/ipcheck`，与 `internal/probe` 平级、相互独立：

* `Source`：数据源接口，每个数据源一个实现，彼此隔离。
* `Manager`：调度、去重、限速、缓存、结果聚合。
* 与 `ProbeManager` 的关系：**独立的 worker 池与队列**，不复用探测队列，避免 IP 检测挤占健康探测。IP 检测的失败**绝不调用** `RecordResult(false)`，不影响节点熔断状态。

接口草案：

```go
package ipcheck

// SourceKind 描述数据源的发起方式。
type SourceKind uint8

const (
	// SourceKindViaNode 必须经由节点出口发起，结果反映目标站点眼中的该节点。
	SourceKindViaNode SourceKind = iota
	// SourceKindByIP 只需要 IP 地址即可查询，可直连或经任意健康节点发起。
	SourceKindByIP
)

type Source interface {
	Name() string
	Kind() SourceKind
	// RequiresKey 为 true 时，未配置密钥的数据源自动禁用。
	RequiresKey() bool
	Check(ctx context.Context, req CheckRequest) (*SourceResult, error)
}

type CheckRequest struct {
	NodeHash node.Hash
	EgressIP netip.Addr
	// HTTP 已绑定到对应出口：via-node 源为经节点的 client，by-ip 源为直连或经健康节点的 client。
	HTTP *http.Client
}
```

**HTTP 能力扩展**：现有 `probe.Fetcher` 只支持 GET 并返回 body 与延迟，而 IP 检测需要状态码、响应头（如 `cf-mitigated`）、自定义请求头，个别源需要 POST。建议在 `internal/netutil` 新增“经 outbound 构造 `*http.Client`”的能力，与现有 `HTTPGetViaOutbound` 并存，不改动探测路径。

### 4.2 归一化结果模型

```json
{
  "egress_ip": "203.0.113.10",
  "checked_at": "2026-10-01T12:00:00Z",
  "summary": {
    "country": "us",
    "city": "Los Angeles",
    "asn": 64500,
    "as_org": "Example Networks",
    "ip_type": "residential",
    "ip_origin": "native",
    "fraud_score": 12,
    "abuse_flags": ["proxy"],
    "cf_challenge": "none",
    "egress_consistent": true,
    "egress_ips_by_site": {
      "cloudflare.com": "203.0.113.10",
      "chatgpt.com": "203.0.113.10",
      "claude.ai": "203.0.113.10"
    },
    "services": {
      "claude": "available",
      "chatgpt": "available"
    },
    "risk_level": "low"
  },
  "sources": [
    {
      "name": "ippure",
      "kind": "via_node",
      "ok": true,
      "checked_at": "2026-10-01T12:00:00Z",
      "expires_at": "2026-10-02T12:00:00Z",
      "data": { "fraud_score": 12, "is_residential": true, "is_broadcast": false },
      "error": ""
    }
  ]
}
```

字段取值约定：

* `ip_type`：`residential` / `datacenter` / `mobile` / `unknown`
* `ip_origin`：`native` / `broadcast` / `unknown`
* `cf_challenge`：`none` / `challenge` / `blocked` / `unknown`
* `services.*`：`available` / `region_blocked` / `risky` / `unknown`
* 任何维度没有数据时一律为 `unknown`（或省略数值字段），不臆造默认值。

聚合规则（草案，待讨论）：

* 各源原始结果完整保留在 `sources`，`summary` 只是派生视图，可随规则调整重新计算。
* `fraud_score`：取各源最大值（保守策略）。
* `ip_type` / `ip_origin`：按可配置的源优先级取第一个非 `unknown` 值。
* `abuse_flags`：各源并集。

### 4.3 结果归属与缓存

* **by-ip 结果按 `egress_ip` 归属**，所有共享该 IP 的节点复用同一份结果。
* **via-node 结果按 `node_hash` 归属**（分流、被盾、服务可用性可能因节点而异），节点出口 IP 变更时自动失效。
* 节点视图展示时合并两者。

持久化到 `cache.db`（弱一致性，与现有节点动态数据一致），复用现有脏集合批量刷盘机制：

```
ip_check_results(
  subject_kind,   -- 'ip' | 'node'
  subject,        -- egress_ip 或 node_hash
  source,         -- 数据源名
  result_json,
  checked_at_ns,
  expires_at_ns,
  PK(subject_kind, subject, source)
)
```

启动时加载未过期结果；节点被物理删除时清理其 `node` 归属的记录，孤立的 `ip` 记录按 TTL 自然淘汰。

### 4.4 调度与限流

* **默认只支持手动触发，不开启后台检测**。开启后台检测需显式配置（渐进式复杂度，且避免未经用户同意向第三方发送出口 IP）。
* **手动**：对单个节点同步检测，可指定数据源，可 `force` 跳过缓存。
* **批量**：按 Platform / 订阅 / 节点列表入队异步执行，通过查询接口查看进度与结果。
* **后台（可选）**：周期扫描（带随机抖动，与现有 scanloop 风格一致），只处理“健康 + 有出口 IP + 结果已过期”的节点；出口 IP 变更时以低优先级入队。
* **限流**：每个数据源独立的令牌桶（QPS）与日配额上限；连续失败达到阈值后暂停该数据源一段时间（源级熔断），不影响其他源。
* **去重**：同一 `egress_ip` 的 by-ip 查询在飞行中合并（singleflight）。100k 节点、10k 个唯一出口 IP 时，只需 10k 次 by-ip 查询。
* **by-ip 源的出口**：默认从 Resin 主机直连；可配置为经健康节点发起，适配主机无法直连外网的部署。

### 4.5 WebAPI（草案）

路径风格与现有 API 保持一致：

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/nodes/{hash}/actions/check-ip` | 同步检测单个节点。请求体可选 `{"sources": [...], "force": false}` |
| GET | `/api/v1/nodes/{hash}/ip-check` | 获取节点的合并检测结果 |
| POST | `/api/v1/ip-checks/actions/check-batch` | 批量入队，参数为 `platform_id` / `subscription_id` / `node_hashes` 之一 |
| GET | `/api/v1/ip-checks/{ip}` | 获取某出口 IP 的 by-ip 结果 |
| POST | `/api/v1/ip-checks/lookup` | 对任意 IP 执行 by-ip 查询（类似现有 GeoIP lookup） |
| GET | `/api/v1/ip-checks/sources` | 列出数据源：是否启用、是否已配置密钥、配额使用情况、源级熔断状态 |

节点摘要模型新增可选字段 `ip_check`（仅含 `summary` 与 `checked_at`）。为避免 100k 节点列表响应膨胀，`GET /nodes` 仅在显式传入 `include=ip_check` 时返回该字段。

错误码沿用现有约定：`400 INVALID_ARGUMENT`、`404 NOT_FOUND`；功能未启用或数据源不可用时返回明确的错误信息，不返回伪造的空结果。

### 4.6 配置（草案）

运行时配置（热更新，存于 `system_config`）：

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `ip_check_background_enabled` | `false` | 是否开启后台周期检测 |
| `ip_check_sources` | 免密钥的 P1 数据源 | 启用的数据源列表 |
| `ip_check_ip_result_ttl` | `24h` | by-ip 结果有效期 |
| `ip_check_node_result_ttl` | `6h` | via-node 结果有效期 |
| `ip_check_consistency_targets` | 见 3.1 | 出口一致性检测的目标列表 |

环境变量（非热更新，适合放密钥）：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `RESIN_IP_CHECK_CONCURRENCY` | `8` | IP 检测 worker 数，独立于 `RESIN_PROBE_CONCURRENCY` |
| `RESIN_IP_CHECK_SCAMALYTICS_USER` / `RESIN_IP_CHECK_SCAMALYTICS_KEY` | 空 | Scamalytics API 凭据 |
| `RESIN_IP_CHECK_IPDATA_KEY` | 空 | ipdata API Key |
| `RESIN_IP_CHECK_CLOUDFLARE_RADAR_TOKEN` | 空 | Cloudflare Radar API Token |

密钥**不进入**运行时配置，因为运行时配置会通过 `GET /system/config` 原样返回。在 `GET /system/config/env` 快照中，密钥需脱敏显示。

### 4.7 WebUI

* 节点列表：新增紧凑徽标列，包括 IP 类型、欺诈分（按阈值着色）、Claude / ChatGPT 可用性、被盾状态、出口不一致警告。
* 节点详情：新增「IP 质量」区块，展示汇总与各数据源明细（含检测时间、错误原因），提供「立即检测」按钮。
* GeoIP 页面：在现有 IP 查询旁增加 by-ip 质量查询入口。
* 中英文 i18n 同步补齐。

### 4.8 与路由的集成（P3）

在数据稳定后，为 Platform 增加 IP 质量过滤条件，例如：

```json
{
  "ip_quality_filters": {
    "ip_types": ["residential"],
    "max_fraud_score": 30,
    "exclude_abuse_flags": ["tor", "known_abuser"],
    "require_services": ["claude"],
    "require_egress_consistent": true,
    "unknown_action": "allow"
  }
}
```

* **严格走冷路径**：IP 检测结果变化时触发 Platform 脏更新，与“出口 IP 变更”触发脏更新的机制一致；过滤器配置变更时全量重建。热路径的 O(1) 选取不受影响。
* `unknown_action`：结果缺失时是否允许节点进入可路由视图。默认 `allow`，否则在未开启后台检测时，新节点会长时间被排除，Platform 可能被清空。
* 节点列表查询同步增加对应过滤参数（`ip_type`、`max_fraud_score` 等）。
* 实现时同步更新 DESIGN.md「Platform 可路由视图 / 过滤条件」章节。

## 5. 数据源接入原则

1. **接入方式优先级**：官方 API / 离线库 > 非官方 JSON 接口 > HTML 解析。浏览器自动化不进入核心。
2. **隔离**：每个数据源独立超时、限速、源级熔断、错误隔离。单个源失败只体现在该源的 `error` 字段中。
3. **最小化外发**：by-ip 源只发送 IP 地址；需要密钥或会外发数据的源默认关闭，由用户显式启用。
4. **可追溯**：每条结果记录数据源、检测时间、有效期，WebUI 明确展示“结果来自第三方，仅供参考”。
5. **可测试**：每个数据源的解析逻辑基于录制的响应样本做单元测试；Manager 基于 fake Source 测试调度、去重、限流、缓存与失效。
6. **合规**：接入前核对各站点的使用条款与 API 许可；对要求署名的数据（如 db-ip Lite）按要求标注来源。

### 5.1 参考数据源评估（初步，实施时逐一核实）

| 数据源 | 接入方式 | Key | 初步结论 |
|--------|----------|-----|----------|
| ip.net.coffee（claude / gpt / ip / dns / 分流） | 未发现公开 API | — | 作为方法论与指标参考，不直接接入 |
| ipsuper.com | 待调研 | — | 作为分流检测方法论参考 |
| db-ip.com | Lite 离线 mmdb（CC BY 4.0）/ 在线 API | 离线库否 | **P1 首选**，复用 GeoIP 更新框架 |
| ippure.com | 非官方 JSON 接口（“查自身”型） | 否 | P1 候选，需确认稳定性与条款 |
| iplark.com | 待调研 | — | 待定 |
| ping0.cc | HTML 解析，常遇 Cloudflare 挑战 | 否 | 可选，默认关闭 |
| scamalytics.com | 官方 API | 是 | P2 |
| ipdata.co | 官方 API（`threat` 字段） | 是 | P2 |
| radar.cloudflare.com | 官方 API | 是（Token） | P2，作为跳盾情报补充；跳盾本身以经节点实测为准 |
| cleanip.io | 待调研 | — | 待定 |
| iptrait.com | 待调研 | — | 待定 |

## 6. 对现有系统的影响

* **热路径**：无影响。
* **健康探测**：无影响。独立 worker 池，IP 检测结果不参与熔断与延迟统计。
* **指标**：IP 检测经节点发起的连接单独计数（例如探测事件新增 `ipcheck` 类型），不混入现有 egress / latency 探测统计。
* **存储**：`cache.db` 增长量约为“唯一出口 IP 数 × 启用源数 × 约 1KB”，外加 via-node 结果。
* **流量**：每次 via-node 检测产生少量 HTTP 请求（一致性检测按目标数计）；后台检测默认关闭。
* **兼容性**：纯增量。所有新 API 为新增路径，节点摘要新增字段需显式 `include`，默认行为不变。

## 7. 分阶段实施计划

| 阶段 | 内容 |
|------|------|
| P0（本 PR） | 提案文档，讨论并确认范围、模型与接口 |
| P1（MVP） | `internal/ipcheck` 框架（Source / Manager / 缓存表 / 限流 / 去重）；经 outbound 的 HTTP client 能力；免密钥数据源：出口一致性、db-ip Lite ASN + 城市、ippure、跳盾实测、AI 服务地区判定；单节点手动检测 API 与查询 API；单元测试 |
| P2 | WebUI 展示与手动检测；批量检测；可选后台调度；需密钥的数据源（Scamalytics、ipdata、Cloudflare Radar）；数据源配额与状态接口；综合风险等级；指标 |
| P3 | Platform `ip_quality_filters`（冷路径脏更新）；节点列表过滤参数；更新 DESIGN.md 与 README |
| P4（调研 / 可选） | DNS 泄露检测方案；外部插件协议（如以 HTTP sidecar 方式接入浏览器类数据源，核心不依赖）；更多数据源（iplark、cleanip、iptrait） |

## 8. 待讨论问题

1. by-ip 查询默认从 Resin 主机直连，还是经健康节点发起？前者简单，但会暴露主机 IP 给第三方；后者消耗节点流量。
2. `fraud_score` 的聚合策略：取最大值、加权平均，还是只展示各源原值不做聚合？
3. Platform 过滤中 `unknown_action` 的默认值是否为 `allow`？
4. 是否接受非官方接口（如 ippure 后端 JSON）进入默认启用的数据源？
5. 是否需要保存检测历史（例如纯净度随时间的变化），还是只保留最新结果？
6. DNS 泄露检测是否值得引入可选的自建组件，如何与「零外部依赖」原则取舍？
7. AI 服务支持地区列表是内置并随版本更新，还是支持从远端定期拉取？

## 9. 参考

* 检测站点：
  * Claude 风险检测：<https://ip.net.coffee/claude/>
  * GPT 风险检测：<https://ip.net.coffee/gpt/>
  * IP 信息：<https://ip.net.coffee/ip/>
  * DNS 泄露检测：<https://ip.net.coffee/dns/>
  * 网站分流查询：<https://ip.net.coffee/>、<https://ipsuper.com/>
  * 欺诈得分：<https://scamalytics.com/>
  * 地理位置 + ASN：<https://db-ip.com/>
  * 滥用标记：<https://ipdata.co/>
  * 纯净度：<https://iplark.com>、<https://ippure.com>
  * 跳盾检测：<https://radar.cloudflare.com/>、<https://cleanip.io/>、<https://www.iptrait.com/>
* 参考实现（tombcato/clash-ip-checker）：
  * <https://github.com/tombcato/clash-ip-checker/blob/main/core/sources/ping0.py>
  * <https://github.com/tombcato/clash-ip-checker/blob/main/core/sources/ippure.py>
  * <https://github.com/tombcato/clash-ip-checker/blob/main/core/sources/browser.py>
* 官方支持地区列表：
  * Anthropic：<https://www.anthropic.com/supported-countries>
  * OpenAI：<https://platform.openai.com/docs/supported-countries>
* 本项目：DESIGN.md「主动探测 / 出口探测」「GeoIP 服务」「Platform 可路由视图」「持久化系统」章节
