# IP 质量能力路线图与验收记录

验证快照：2026-10-14，PR12–15、PR17–24 已合并；本切片接入默认关闭的来源 HTTP 尝试限制与保护熔断，且同步单次/批量抑制语义。最终 CI 与合并状态以各 PR 描述为准；早期提案中的「仅含文档」不是当前状态。

本 fork 未启用 Issues，本轮只用仓库文档与 PR 跟踪，不修改仓库设置。P1 已通过 [PR1](https://github.com/58cdn/Resin/pull/1) 合并；PR2–11 已合并，不重复开发。P2–P4 尚未全部完成。

## 当前分支和验证关系

| 记录 | 最终 merge / 当前基础 | 状态与验证范围 |
| --- | --- | --- |
| PR1 | `4a04becce1793c64958e568aac145a2f48ab0b49` | 已合并的 P1 基线；不是部署记录 |
| [PR12](https://github.com/58cdn/Resin/pull/12) | `c0ea4155847dd3b6e61125d41d7aae79e6dfbb07` | 07:16:01 UTC 已合并；Windows reset 修复与 lease 测试时钟修正；[临时辅助 PR 的准确候选 CI 38525264468](https://github.com/58cdn/Resin/actions/runs/38525264468) 双平台通过，辅助 PR16 已关闭且未合并 |
| [PR13](https://github.com/58cdn/Resin/pull/13) | `4bb9188eb966d873174f54f5368be20d4603a4d8` | 07:36:48 UTC 已合并；[最终自身 CI 38527442777](https://github.com/58cdn/Resin/actions/runs/38527442777) 双平台通过；合并后 Windows 暴露 metrics 测试跨秒缺陷，修复见 PR14 |
| [PR14](https://github.com/58cdn/Resin/pull/14) | `590ab517cbb346f769ebe4563685ad4afc67b7ed` | 08:15:01 UTC 已合并；节点详情面板与 metrics 测试修复；[最终自身 CI 38531553063](https://github.com/58cdn/Resin/actions/runs/38531553063) 双平台 test/race/build/vet、4 项前端测试及构建通过；另有 14 项本地 mock 浏览器检查 |
| [PR15](https://github.com/58cdn/Resin/pull/15) | `def0caddb9bc6db90b88504a3e4f7274adf31595` | 已合并；[批量缓存 API](ip-quality-batch-cache.zh-CN.md)；最终自身 CI 38534451747 与 [实际 master CI 38536711116](https://github.com/58cdn/Resin/actions/runs/38536711116) 双平台通过 |
| [PR17](https://github.com/58cdn/Resin/pull/17) | `fbe76a6d0c2a17772a874381dcafbe7c56630ced` | 10:41:09 UTC 已合并；列表缓存 UI，独立复审与 [自身 CI 38540128111](https://github.com/58cdn/Resin/actions/runs/38540128111) 通过；[实际 master CI 38551296373](https://github.com/58cdn/Resin/actions/runs/38551296373) 双平台通过 |
| [PR18](https://github.com/58cdn/Resin/pull/18) | `d4e4dfee69a7d170480cb41601e8d8d138d0d516` | 11:10:17 UTC 已合并；[独立只读页面](ip-quality-byip-cache.zh-CN.md)，最终独立复审及 [自身 CI 38551995468](https://github.com/58cdn/Resin/actions/runs/38551995468) 双平台通过；[实际 master CI 38554697676](https://github.com/58cdn/Resin/actions/runs/38554697676) 双平台通过：20 项单测、36 组浏览器检查及 Go test/race/build/vet |
| [PR19](https://github.com/58cdn/Resin/pull/19) | `7f27cbb00b5263b1450bfc2d63e0ae05be82c24e` | 11:37:21 UTC 已合并；[显式准备与检测](ip-quality-manual-check.zh-CN.md)；[最终自身 CI 38555153025](https://github.com/58cdn/Resin/actions/runs/38555153025) 双平台通过：28 项单测、54 组浏览器检查、Go test/race/build/vet；[实际 master CI 38557881711](https://github.com/58cdn/Resin/actions/runs/38557881711) 以运行结果为准 |
| [PR20](https://github.com/58cdn/Resin/pull/20) | `5529c29d93fbeb70d0854ae6e63bb64f452b7ad5` | 12:07:56 UTC 已合并；[显式节点任务、进度与取消](ip-quality-batch-tasks.zh-CN.md)；[自身 CI 38558713134](https://github.com/58cdn/Resin/actions/runs/38558713134) 与 [实际 master CI 38561534901](https://github.com/58cdn/Resin/actions/runs/38561534901) 双平台通过：28 项单测、54 组浏览器检查、Go test/race/build/vet |
| [PR21](https://github.com/58cdn/Resin/pull/21) | `1b9e6f85a89f45548996ddd211f27b737b1634b1` | 13:13:23 UTC 已合并；[显式批量任务页面](ip-quality-batch-ui.zh-CN.md)，[自身 CI 38566762746](https://github.com/58cdn/Resin/actions/runs/38566762746) 双平台通过：41 项单测、72 组浏览器检查、Go test/race/build/vet；[实际 master CI 38570000586](https://github.com/58cdn/Resin/actions/runs/38570000586) 以运行结果为准 |
| [PR22](https://github.com/58cdn/Resin/pull/22) | `025feb37e73656751e2a15abc4d43437345bbc4b` | [实际节点拨号累计与只读 API](ip-quality-dial-metrics.zh-CN.md)；独立双向复审、受控 mock/回环测试及自身 CI38571118063 双平台通过；实际 master CI38574290771 结果记录于 PR 描述 |
| [PR23](https://github.com/58cdn/Resin/pull/23) | `6cc744eac4df36af208118336d957f1a2def7360` | [默认关闭的显式节点定期检测](ip-quality-periodic.zh-CN.md)，最终自身 CI38577302553 双平台通过；实际 master CI38580944481 结果见 PR 描述 |
| [PR24](https://github.com/58cdn/Resin/pull/24) | `190d6c242fbe69c8de619b4aefd8afaeed751755` | [HTTP 尝试控制器及离线验证](ip-quality-request-guards-core.zh-CN.md)；此独立核心 PR 没有生产调用；自身 CI38582284619 双平台通过，实际 master CI 见 PR 描述 |
| 本请求控制运行接入切片 | 依赖 PR24 独立核心 | [默认关闭的来源请求控制](ip-quality-request-guards.zh-CN.md)；进程寿命、保存后提交、逐 HTTP 尝试准入及单次/批量/UI 抑制语义；未在运行服务启用 |

每次合并均重新核对实际 base/head、准确候选及最终树，不以旧 merge ref 的成功替代新代码验证。各切片按依赖顺序合并；PR18 已修复组合后重复的共享翻译键。现有四套浏览器检查保留，并为运行接入增加抑制场景。用户已授权通过最终检查后自动合并；实际合并仍以远端结果为准。

[PR13 合并后 CI 38529466438](https://github.com/58cdn/Resin/actions/runs/38529466438) 的 Windows 失败保留为失败记录：一秒桶在数据库恢复期间合法新增空桶，测试却查询全部时间并假定只有一行。延迟 overlay 已复现；PR14 只修改测试，按原桶时间严格验证并确定性检查新空桶，100 次目标测试、20 次 metrics 全包 race 及最终双平台 CI 通过。没有修改生产逻辑、放宽断言或跳过检查。

## 切片顺序与剩余验收

| 顺序 | 范围 | 当前状态 | 进入下一步的依据 |
| --- | --- | --- | --- |
| 1 | 节点详情缓存、来源错误与状态、中英文 | PR14 已合并 | 最终自身双平台 CI 和受控浏览器检查通过 |
| 2 | 节点缓存批量读取 API | PR15 已合并；50 项上限、规范化去重、限流、取消、逐项失败、原始来源和 unknown | fake host 和最终双平台 CI 通过；不发检测请求 |
| 3 | 节点列表紧凑徽标 | PR17 已合并 | 批量只读契约避免每行请求；进度/取消、过期/出口变更和 unknown |
| 4a | by-IP 只读缓存页面 | PR18 已合并；仅复用已合并 GET | IPv4/IPv6/非法输入、404 模糊语义、来源失败、过期、取消/导航/晚到响应、键盘及移动布局；自身证据见 PR |
| 4b | 显式单次检测控件 | [PR19 已合并](ip-quality-manual-check.zh-CN.md)，复用 P1 单次 API | 明确准备对象并选择来源后仅执行一次；取消/截止/晚到响应、节点快照变化、部分失败与缓存核对已有受控回归；真实验证另行确认节点与目标 |
| 5a | 批量检测 API、任务进度与取消 | [PR20 已合并](ip-quality-batch-tasks.zh-CN.md)，显式 Hash 与 sources，4 活动任务、2 worker、32 总记录、30 分钟截止 | 容量、保留期内幂等、终态、取消/截止、配置替换/停止、缓存/force、共享检测隔离及最终自身/实际 master 双平台 CI 通过 |
| 5b | 批量任务界面 | [PR21 已合并](ip-quality-batch-ui.zh-CN.md)，明确 Hash/来源、固定请求、只读恢复、有限观察、独立取消 | 独立模型与浏览器回归覆盖丢失响应重试、归属与竞争、超时/限流/鉴权、历史结果语义；最终自身 CI 双平台通过 |
| 6a | 默认关闭的显式节点定期检测 | [PR23 已合并](ip-quality-periodic.zh-CN.md)；显式 1–50 Hash、可用来源与间隔，成功保存后启动 | 首次完整等待、固定延迟、周期预算与公平游标、停用/重配/停止取消、缓存与共享请求；最终自身双平台 CI 通过 |
| 6b | 全节点池周期扫描 | 未实现；当前不会扩展节点池、平台、订阅或过滤结果 | 另行定义有界枚举、快照、调度公平性与节点资格变化，不能把 6a 当作全池扫描完成 |
| 7a | 每来源 HTTP 尝试控制器 | [PR24 独立核心](ip-quality-request-guards-core.zh-CN.md)；默认关闭的令牌桶、UTC 日软预算和熔断状态机 | 固定来源、配置不重置消耗、时间回拨、原子准入、一次完成、半开唯一及旧代完成隔离，确定性离线验证 |
| 7b | 来源请求控制运行接入 | [本切片实现](ip-quality-request-guards.zh-CN.md)，默认关闭且未启用实际配置 | 进程寿命与保存后提交、类型化 HTTP/Host 结果、局部抑制不污染缓存、单次/批量及严格前端解析同步；真实供应商计费硬额度另需持久化和账号规则 |
| 8 | Scamalytics / ipdata / Radar 等来源 | 未实现；不读取或创建密钥 | 核实官方接口、许可、字段、配额、凭据输入与脱敏；先做固定响应和传输 mock |
| 9 | 可公开审查、配置和测试的风险评分 | 未实现；`risk_level` 保持 unknown | 先有可追溯的规则、阈值依据及版本；保留原始来源、冲突和缺失，不能把缺数据、0 或无标记判成低风险 |
| 9b（本轮） | IP 检测专用指标 | [实际节点拨号累计与只读 API 已实现](ip-quality-dial-metrics.zh-CN.md)；结果/任务/来源指标和历史序列仍待独立定义 | DNS 安全校验之后按实际 outbound 调用计数；并发一致快照、一次完成、取消/deadline 分类及旧指标隔离。拨号成功不代表 HTTP 或质量成功，读取与缓存命中不新增拨号 |
| 10 | P3 Platform / 节点质量过滤 | 未实现 | P2 结果语义可靠后另立切片；`unknown_action` 默认 allow，只在冷路径重建，热路径不查库/联网 |
| 11 | P4 DNS leak / 自建权威 DNS / 外部 capability | 未实现 | 独立需求、威胁模型、协议和授权调研；不能借 UI 或缓存工作启动外部探测 |

批量任务 API 和界面分别审查。全池扫描、供应商账号硬配额、评分和过滤不能因 UI/API、显式节点定期检测或进程内请求控制已实现而被视为完成。

## P1 保留的边界

- ippure 只报告 unavailable；接口和许可未确认，不发请求。
- Cloudflare challenge / AI region 有显式目标适配器及固定响应测试；不内置未经确认的目标、地区名单，不保证账户或推理可用性。
- DB-IP 复用离线库实际具备的字段；不自动补充 City/ASN 库或在线数据源。
- 来源的原始结果是证据，summary 是派生视图；未知布尔/数字为 null，分类为 unknown。
- 插件默认关闭；显式开启定期检测也需先等待完整间隔。缓存读取不探测真实节点，不改变健康、路由或节点熔断。

本路线图不包含此前冻结的资源控制大改。没有生产部署、发行、真实节点检测或凭据操作；这些都不能由本轮测试成功推导出来。
