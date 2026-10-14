# IP 检测节点拨号指标

管理员可读取 `GET /api/v1/metrics/ip-checks/dials`。请求沿用管理 API 认证，不支持查询参数，响应带 `Cache-Control: no-store`。读取只返回内存快照，不会启动检测、访问节点或启用插件；插件从未启用时也返回合法的零计数。

## 计数口径

这些指标累计于当前进程的 metrics Manager，`started_at` 表示累计起点，`observed_at` 表示读取时间；均为 UTC 时间。进程重启会归零，插件禁用、重新启用或修改配置不会重置。`scope` 固定为 `process`，没有按节点、出口 IP、来源或平台分类，也不保存历史序列。

| 字段 | 含义 |
| --- | --- |
| `dials_started` | 已实际调用 IP 检测节点 outbound 的 `DialContext` 次数 |
| `dials_completed` | 上述拨号调用已返回的次数 |
| `dials_succeeded` | 拨号返回错误为 nil 的次数 |
| `dials_failed` | 拨号返回非取消错误的次数，包含 deadline exceeded |
| `dials_canceled` | 拨号返回错误可由 `errors.Is(err, context.Canceled)` 识别的次数 |
| `dials_in_flight` | 已开始、尚未返回的拨号调用数；不是存活 TCP 连接数 |

同一快照保证 `dials_started = dials_completed + dials_in_flight`，以及 `dials_completed = dials_succeeded + dials_failed + dials_canceled`。结果按实际返回的错误分类：如果取消请求与拨号成功同时发生，但拨号返回 nil，仍计为拨号成功。

只在公共 DNS 地址安全校验通过后、调用节点 outbound 前计数。请求校验、节点不可用或 DNS 失败/拒绝均不增加这些指标。离线 GeoIP、缓存读取，以及缓存命中后没有实际拨号的检测请求也不计数。共享检测的多个等待者不会按等待者人数计数；HTTP transport 若实际发起多个拨号，每次调用分别计数。

拨号成功不等于 TLS 握手成功、HTTP 状态成功、来源解析成功、出口一致或 IP 质量良好。HTTP 503、无效内容或后续读超时可能发生在一次成功拨号之后。任务完成数、来源成功率、缓存命中率和质量风险分数需要独立定义，不能从这些计数推导。

## 隔离与验证

计数器不改变既有 egress/latency 探测、用户请求、流量、连接峰值、健康状态或路由指标；本拨号指标切片不新增数据库表、调度器、外部目标、凭据读取或标签基数。后续[显式节点定期检测](ip-quality-periodic.zh-CN.md)仍默认关闭，按实际拨号复用这些计数。

测试覆盖累计与归零起点、并发快照一致性、重复完成去重、取消与 deadline 分类、原有指标隔离、管理员认证与参数拒绝，以及通过受控本地 TLS 服务验证“HTTP 503 仍可对应一次成功拨号”。所有网络测试使用 mock 或回环服务，不访问真实节点和外部观测目标。
