# IP 质量检测（手动 API 与缓存查看）

`resin.ip-quality` 是由现有 Plugin Manager 管理的首方 builtin。默认禁用；启用本身不发起检测，后台检测不支持。只有管理员显式调用检测 API 才执行所选数据源。结果仅供参考，不代表服务可用性或风控保证。

## 启用与配置

通过现有插件管理界面或 `PATCH /api/v1/plugins/resin.ip-quality`：

```json
{"enabled":true,"config":{"enabled_sources":["egress-consistency","db-ip"],"ip_result_ttl":"24h","node_result_ttl":"6h","request_timeout_ms":10000,"consistency_targets":["https://cloudflare.com/cdn-cgi/trace"],"background_enabled":false}}
```

若只需要离线查询，将 `enabled_sources` 设为 `["db-ip"]`。检查出口会向配置的目标发送不带用户凭据的 GET；目标能看到节点出口 IP。不会自动检测用户节点或访问其他情报站点。`background_enabled:true`、未知字段、非法 TTL/超时/目标均被拒绝。

TTL 限制为 1 秒至 30 天；请求超时 100–60000 ms；目标 1–8 个，每个不超过 2048 字节；只允许公网 HTTPS。宿主拒绝所有重定向，验证全部 DNS 回答并固定公网地址后经节点连接，保留域名 TLS 校验。此安全策略使用宿主 DNS，因此目标 CDN 地址可能与节点远端 DNS 选择不同。

可选 `max_concurrent_checks`（默认 8，1–64）、`max_cache_entries`（默认 4096，1–100000）、`source_priority` 控制冷路径资源与摘要优先级。

配置和 JSON 观测对象拒绝重复字段，包括大小写或 Unicode 大小写别名。IP echo 的 trace 必须完整符合 `key=value` 格式，混杂 HTML、重复 `ip` 或矛盾 JSON 不会作为有效出口证据。AI 可用性要求合法的 `application/json` 媒体类型（可带参数），不接受 `application/jsonp` 或格式损坏的 Content-Type。

## API

所有接口沿用管理员鉴权、请求体限制和错误格式。

- `POST /api/v1/nodes/{hash}/actions/check-ip`：`{"sources":["egress-consistency","db-ip"],"force":false}`。同步手动检测。
- `GET /api/v1/nodes/{hash}/ip-check`：只读节点合并缓存。
- `GET /api/v1/ip-checks/{ip}`：只读该公网 IP 的 by-IP 缓存。
- `POST /api/v1/ip-checks/lookup`：`{"ip":"8.8.8.8","sources":["db-ip"],"force":false}`。只允许 by-IP 数据源。
- `GET /api/v1/ip-checks/sources`：查看启用状态、类型及不可用原因。

非法 IP/请求返回 400，节点不存在或未检测返回 404，节点无公网出口返回 409，插件未运行返回 503。数值/布尔未知为 null，分类未知为 `unknown`；source unavailable 不伪造成功。返回的原始 source 结果是事实来源，summary 是派生视图。

## WebUI 缓存查看（P2 首个切片）

节点详情抽屉中的「IP 质量缓存」面板读取该节点的已有缓存与数据源状态。打开面板或点击「刷新缓存」只调用 GET，不会启动检测、后台任务或自动启用插件。管理员仍通过上述 API 显式执行检测。

面板保留 unknown/null 语义，显示来源观测、时间、错误与原始数据；来源请求成功不等于节点健康或账户可用。空滥用标记不表示无风险。缓存出口与节点出口不匹配时隐藏旧结果；任一来源到期后隐藏聚合摘要，等待刷新获取当前有效缓存。关闭面板停止相关查询和本地到期计时。

此切片包含中英文界面，不包含列表徽标、by-IP 查询页或手动检测按钮。

## 已实现的数据源

### egress-consistency

经指定节点并发读取配置目标的 trace 或 IP echo，记录各站点看到的出口。单个目标失败保留其他目标结果。只有至少两个独立目标均成功返回相同 IP，才能证明这次观测一致；只配置一个默认 Cloudflare trace 时一致性为未知。目标失败和超时不会改变节点健康、熔断、延迟或路由。

### db-ip（离线）

复用现有 GeoIP 服务及其校验、原子替换和关闭流程，不增加自动网络下载或在线 by-IP 查询。数据库含有的 country、city、ASN、AS organization 可读取；现有 country 库通常只含国家，其余保持未知。未安装/未覆盖地址返回 unavailable，不会伪造城市、ASN 或 IP 类型。

使用 DB-IP Lite 数据时，来源为 [DB-IP](https://db-ip.com/)，数据许可为 [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/)。本功能不会把其他提供商的库识别为 DB-IP 来源证明；管理员需保留实际使用库的来源与许可。更完整的 City/ASN 数据库自动分发不在本阶段交付范围。

### Cloudflare challenge 与 AI region（显式配置）

两个源默认关闭、没有默认目标；仅加入 `enabled_sources` 还不够，必须显式配置目标。
所有目标沿用公网 HTTPS、DNS 固定、拒绝重定向与响应体上限策略，经指定节点访问。
下面的 example.com 只是配置格式示意，须替换为你已确认可使用的观测端点：

```json
{
  "enabled_sources": ["db-ip", "egress-consistency", "cloudflare-challenge", "ai-region"],
  "cloudflare_challenge_urls": ["https://example.com/check"],
  "ai_region_targets": {
    "chatgpt": {"url": "https://example.com/chatgpt-observation", "supported_countries": ["US", "GB"]},
    "claude": {"url": "https://example.com/claude-observation"}
  }
}
```

`cloudflare_challenge_urls` 最多 8 个目标。`cf-mitigated: challenge` 是挑战证据；
403/503 的挑战页面标记也可识别。其他 403 为 blocked，正常 2xx 为 none，模糊情况 unknown。
部分失败保留其他目标的有效证据，不把失败当正常。

`ai_region_targets` 仅接受 claude/chatgpt，每项一个 URL 与可选的明确支持地区列表。
从成功 trace 响应读取 `loc`，已知地区不在用户提供的列表时为 region_blocked；
地区在支持列表中仍为 unknown，不能据此保证登录、账户或推理可用。
明确的地区阻断响应为 region_blocked，挑战/429 为 risky。通用成功 HTML 不会判定 available。
只有配置端点返回 application/json 且明确给出 `{"service":"chatgpt","available":true}`
这样的匹配服务契约才判定 available；这是本插件支持的观测契约，不声称是供应商官方 API。
管理员负责选择合法稳定端点、维护地区列表并理解第三方证据的局限。
目标配置可由管理员插件 API 读取，URL 中不要放密钥、会话或其他凭据；本阶段不支持带认证的探测。

## 缓存与生命周期

by-IP 缓存按规范化出口 IP/source 共享；via-node 按 node hash/source 隔离并绑定观测出口。有效缓存直接复用；force 跳过缓存但合并正在运行的相同检查。配置更新替换引擎，禁用/Manager Stop 取消旧引擎任务。出口快照同时绑定最近成功探测的时间版本，新的成功出口采样（包括 IP 未变）会保守失效 via-node 旧缓存，从而覆盖 A→B→A 回切；删除节点清理 node 记录；IP 记录按 TTL 清理。

结果使用 cache.db 的增量迁移和现有批量刷盘/关闭流程持久化。不在代理热路径读库、执行检测或复杂聚合。

## 明确未实现

本提交实现 P1 插件框架与可独立验证的数据源。以下仍有明确边界：

- ippure：仅报告 unavailable；接口与许可尚未确认，不发请求。
- Cloudflare challenge、AI region：显式配置端点适配与 fixtures 已实现；不内置未经确认的第三方端点或官方地区名单，也不能检测账户级可用性。
- 不读取 Scamalytics/ipdata/Radar 密钥，也不调用其 API；后续需另行实现和测试。配置中不接受密钥字段。
- P2 剩余：节点列表质量徽标、by-IP 查询、手动检测控件、批量/后台任务、配额/源熔断、风险评分、需密钥情报源。节点详情只读缓存面板已实现。
- P3：Platform 过滤与节点列表质量过滤。
- P4：DNS leak、自建权威 DNS、外部 capability 协议调研。
- 不引入浏览器自动化或未确认条款的 HTML 抓取。

测试全部使用 fake host、固定响应或 httptest，不要求真实节点、API key 或公网检测端点。
