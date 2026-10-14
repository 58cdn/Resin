# 节点 IP 质量缓存批量读取（P2 独立切片）

本接口供节点列表等调用方一次读取已有缓存。它是同步、只读缓存查询；不会启动 source、网络检测、后台任务或风险评分。插件继续默认关闭，入口沿用管理员鉴权。实现进度见[路线图](ip-quality-roadmap.zh-CN.md)。

## 请求与响应

`POST /api/v1/ip-checks/cache/nodes`

```json
{"node_hashes":["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"]}
```

- 使用 POST 仅为了携带有界的批量选择；不接受 `force`、`sources`、目标 URL 或调度参数。
- 请求体最多 16 KiB，若部署原有 API body limit 更小则遵守更小值。未知/重复字段、非精确 `node_hashes` 字段名（含大小写别名）、非对象、尾随 JSON、空列表或非法 hash 返回 400。
- 原始列表 1–50 项，先完整校验，再把合法 32 字符 hex hash 规范为小写并去重；即使全是重复项，51 项也返回 400。
- 按首次出现顺序返回每个唯一节点；`total` 为去重后的节点数。
- HTTP 200 只说明批量读取完成，不说明所有节点已有缓存，更不说明节点健康或安全。

每项恰好有 `result` 或 `error`：

```json
{
  "items": [
    {
      "node_hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "error": {"code":"NOT_FOUND","message":"IP quality not checked: no unexpired results"}
    },
    {
      "node_hash": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "error": {"code":"CONFLICT","message":"node has no public egress IP; probe its egress IP first"}
    }
  ],
  "total": 2
}
```

缓存命中时，`result` 与 `GET /api/v1/nodes/{hash}/ip-check` 的公开结果结构相同：出口、观测时间、summary 和完整 source 结果；不返回内部 `config_fingerprint`。source error/unavailable、部分观测、false、0 和 null 均原样保留，不补造 risk_level 或成功结论。

## 限流、取消和缓存语义

- 该路由每个 API server 实例共用令牌桶：补充速率 2 请求/秒、突发容量 2，最多同时处理 2 批。超限立即返回 429 `RATE_LIMITED` 和 `Retry-After: 1`；无排队、无额外 worker。所有调用者共享额度，无效但已接纳的请求也消耗令牌。重试时间是提示，不保证下一次必定接纳。
- 原生 HTTP 路由从进入 handler 起设置本请求的 5 秒读取截止、15 秒写入截止，避免慢请求体/慢接收者无限占用并发名额，不改变服务器全局配置。每批顺序读缓存，在请求体验证后另设 5 秒执行预算；更早的客户端取消/超时会传播到缓存读取。处理和发送结果前检查取消；读取/执行取消或超时返回 503 且不输出部分 items。网络断开或写入截止可能导致连接关闭或响应不完整，调用方不得使用不完整 JSON。没有脱离请求存活的任务或自动重试。
- 每次读取通过现有 ControlPlaneService 解析当前插件，不持有跨请求引擎。插件禁用、停止或不可用导致整批 503，已收集的部分结果丢弃。配置、来源 TTL、出口 IP/版本校验沿用单节点缓存读取。
- 批次是逐项读取，不是所有节点的事务快照。调用方必须保留 `expires_at` 并随时间隐藏过期摘要，不能将返回时间当作新的 `checked_at`，也不能延长原始 TTL。
- HTTP 响应使用 `Cache-Control: no-store`。未检测、过期或现有缓存加载失败不会自动检测补齐。底层现有 cache loader 将存储加载错误按 miss 处理，本接口不会伪称此时已有有效结果。
- 单节点公开结果 JSON 超过 64 KiB 时返回该项 `RESULT_TOO_LARGE`，不截断来源证据、不删除缓存；调用方可改用单节点 GET。最多 50 份 64 KiB 结果加固定信封，正常总响应小于 3.2 MiB。这是编码后的保留/输出限制，不是峰值内存承诺：当前节点的完整对象仍需先编码，现有持久化层另有每来源大小限制。

这是该缓存路由的接纳限制，不是全局资源配置或第三方来源 QPS/配额实现；其他已有接口的行为不变。

## 失败与 unknown

| 范围 | 状态 / code | 意义 |
| --- | --- | --- |
| 整批 | 400 / INVALID_ARGUMENT | 请求格式、数量或任一 hash 非法；没有读取任何节点 |
| 整批 | 401 / UNAUTHORIZED | 管理员鉴权失败 |
| 整批 | 413 / PAYLOAD_TOO_LARGE | body 超过较小的路由/服务器限制 |
| 整批 | 429 / RATE_LIMITED | 路由令牌或并发已满，未开始缓存读取 |
| 整批 | 503 / SERVICE_UNAVAILABLE | 插件不可用，或请求取消/超时；无部分成功信封 |
| 单项 | NOT_FOUND | 节点不存在，或没有仍有效的缓存；message 区分原因 |
| 单项 | CONFLICT | 没有公网出口，或读取中出口版本变化；不返回旧证据 |
| 单项 | INTERNAL | 读取/编码失败，返回固定消息，不泄露底层路径或异常 |
| 单项 | RESULT_TOO_LARGE | 结果超过批量大小边界，可读单节点缓存 |
| result 内 | source status=error/unavailable | 有可展示的来源失败证据；不等于请求级失败或“低风险” |

429/503 是整批状态；NOT_FOUND/CONFLICT 等单项失败不会抹掉其他节点的有效证据。任何缺失字段仍使用原来的 null/unknown 语义。没有新的风险判定规则。

## 验收范围

测试全部用合成 hash、fake host、内存固定响应和 httptest，不使用真实节点、真实密钥或生产访问。覆盖全输入校验、50 项边界、大小写去重/顺序、鉴权和 body limit、限流及并发释放、取消/超时、整批中断、逐项错误、证据脱敏与大小上限，以及出口版本和插件配置/禁用生命周期。继承的 engine 测试继续验证 TTL/持久化/源错误语义。

本切片不含列表 UI、批量检测任务、进度 API、后台调度、source 配额或评分；这些都有[后续验收项](ip-quality-roadmap.zh-CN.md#切片顺序与剩余验收)。实际本地命令、云端运行和最终 HEAD 记录在本 PR 描述中，不能把尚未运行的检查写成通过。
