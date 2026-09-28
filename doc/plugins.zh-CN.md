# Resin 插件

[English](plugins.md)

插件可以在不修改 Resin 源码的前提下扩展其功能。插件可以：

- **在路由前检查代理请求**：拒绝请求、改写路由身份（Platform / Account），或设置/移除发往上游的请求头；
- **接收事件**：请求完成（`request.finished`）以及粘性租约生命周期变化（`lease.created`、`lease.replaced`、`lease.removed`、`lease.expired`）。

插件分为两类，均可在 WebUI 的 **插件** 页面或通过 `/api/v1/plugins` 管理：

| 类型 | 运行位置 | 可用性 |
| --- | --- | --- |
| **内置插件（Builtin）** | 编译进 Resin，进程内运行 | 始终可用，默认禁用 |
| **包插件（Package）** | 由 Resin 启动的子进程，可用任意语言编写 | 仅在 `RESIN_EXTERNAL_PLUGINS_ENABLED=true` 时可用 |

包插件可以通过上传压缩包、复制目录到插件目录，或从 **插件市场**（通过 HTTP 提供的静态 JSON 索引）安装。

## 内置插件

| ID | 能力 | 说明 |
| --- | --- | --- |
| `resin.access-control` | 请求钩子 | 按顺序匹配的允许/拒绝规则，可按客户端 CIDR、平台、账号（支持通配符）、目标主机（精确、glob、CIDR、`<local>`）和代理类型匹配 |
| `resin.header-rewrite` | 请求钩子 | 按规则设置/移除上游请求头；值中可使用 `${account}`、`${platform}`、`${client_ip}`、`${target_host}` |
| `resin.webhook` | 事件 | 将事件批次以 `{"source":"resin","events":[...]}` 的形式 POST 到 HTTP 端点 |

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RESIN_PLUGIN_DIR` | `<RESIN_STATE_DIR>/plugins` | 包插件目录，每个插件 id 一个子目录。插件数据位于 `<RESIN_PLUGIN_DIR>/.data/<id>` |
| `RESIN_EXTERNAL_PLUGINS_ENABLED` | `false` | 是否允许包插件、上传和插件市场 |
| `RESIN_PLUGIN_MARKETPLACE_URLS` | 空 | 插件市场索引 URL，用 `;`、`,` 或换行分隔 |

每个插件的设置（保存在 `state.db` 中，可运行时修改）：

| 设置 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 新发现或新安装的插件默认禁用 |
| `priority` | `0` | 请求钩子按优先级从高到低执行（相同时按 id 排序）。范围 -10000..10000 |
| `timeout_ms` | `1000` | 单次请求钩子超时。范围 10..60000 |
| `fail_closed` | `false` | 钩子出错或超时时：`false` 跳过该插件，`true` 以 `503 PLUGIN_ERROR` 拒绝请求 |
| `config` | 清单中的默认值 | 插件专属的 JSON 对象，按 `config_fields` 校验并热更新 |

## 安全模型

包插件以与 Resin 相同的操作系统用户和权限运行。请只安装你信任的插件。

- 包插件、上传和插件市场 **默认关闭**。
- 插件进程 **不会** 继承 `RESIN_*` 环境变量（其中包含管理令牌和代理令牌），只会收到 `RESIN_PLUGIN_ID`、`RESIN_PLUGIN_DIR` 和 `RESIN_PLUGIN_DATA_DIR`。
- `Proxy-Authorization` 永远不会传给插件。插件不能修改 `Host`、`Content-Length`、`Transfer-Encoding`、`Connection`、`Upgrade`、`TE`、`Trailer` 或 `Proxy-Authorization`；非法的请求头名称或值会被忽略。
- 事件中不包含请求/响应的正文或请求头。
- 压缩包会检查路径穿越、绝对路径、链接以及大小限制（下载 100 MiB、解压后 512 MiB、4096 个条目）。插件市场的制品必须与其 `sha256` 匹配。
- 展示插件市场 URL 时会隐去其中的凭据和查询字符串。

## 请求钩子链

对每个 HTTP 正向代理、CONNECT、反向代理和 SOCKS5 请求，在代理认证之后、路由之前，Resin 会按优先级依次调用每个已启用的请求插件：

1. 插件收到 `RequestInfo`。其中的 Platform、Account 和请求头已包含更高优先级插件所做的修改。
2. `reject` 决策会终止链。HTTP 客户端收到插件指定的状态码（默认 403）和消息，并带有 `X-Resin-Error: PLUGIN_REJECTED` 与 `X-Resin-Plugin: <插件 id>` 响应头。SOCKS5 客户端收到回复 `0x02`（规则不允许连接）。
3. `continue` 决策可以覆盖 Platform/Account（用于路由和请求日志），并设置/移除请求头。请求头修改只对反向代理和普通 HTTP 正向代理的上游请求生效。
4. 错误和超时会计入插件统计。开启 `fail_closed` 时请求以 `503 PLUGIN_ERROR` 失败（SOCKS5：一般性失败 `0x01`）；否则跳过该插件。

没有启用任何请求插件时，热路径的开销只是一次原子读取。

## 事件

事件异步投递，不会拖慢代理。每个订阅了事件的插件都有一个容量为 4096 的有界队列；事件以最多 256 条为一批发送，或每秒发送一次。队列满时新事件会被丢弃，并计入 `events_dropped`。

`request.finished` 数据：

```json
{
  "started_at": "2026-09-28T08:00:00.123Z",
  "proxy_type": "reverse",
  "client_ip": "10.0.0.8",
  "platform_id": "8b1c...",
  "platform_name": "openai",
  "account": "user_tom",
  "target_host": "api.example.com",
  "target_url": "https://api.example.com/v1/models",
  "node_hash": "9f2c...e1a0",
  "node_tag": "sub-A/HK-01",
  "egress_ip": "1.2.3.4",
  "duration_ns": 183000000,
  "first_byte_ns": 95000000,
  "net_ok": true,
  "http_method": "GET",
  "http_status": 200,
  "ingress_bytes": 512,
  "egress_bytes": 4096
}
```

失败的请求还会带有 `resin_error`、`upstream_stage` 和 `upstream_err_msg`。

`lease.*` 数据：

```json
{
  "platform_id": "8b1c...",
  "platform_name": "openai",
  "account": "user_tom",
  "node_hash": "9f2c...e1a0",
  "egress_ip": "1.2.3.4",
  "created_at": "2026-09-28T08:00:00Z"
}
```

## 编写包插件

插件包是一个目录（或该目录的 `.zip` / `.tar.gz`），`plugin.json` 清单位于其根目录，或位于唯一的顶层目录中。

```
example.rate-limit/
├── plugin.json
└── bin/
    └── rate-limit
```

### 清单（`plugin.json`）

```json
{
  "schema_version": 1,
  "id": "example.rate-limit",
  "name": "Rate Limit",
  "version": "1.0.0",
  "description": "Token-bucket rate limit per account.",
  "author": "you",
  "homepage": "https://example.com",
  "license": "MIT",
  "min_resin_version": "1.0.0",
  "capabilities": {
    "request_hook": true,
    "events": ["request.finished", "lease.*"]
  },
  "config_fields": [
    {"name": "rate_per_second", "label": "Requests per second", "type": "number", "default": 10, "required": true}
  ],
  "runtimes": {
    "linux-amd64": {"command": ["bin/rate-limit"]},
    "any": {"command": ["python3", "main.py"], "env": {"PYTHONUNBUFFERED": "1"}}
  }
}
```

| 字段 | 说明 |
| --- | --- |
| `schema_version` | 必须为 `1` |
| `id` | 1-64 个 `a-z 0-9 . _ -` 字符，首尾必须是字母或数字，且必须与目录名一致。内置插件的 `resin.*` id 被保留 |
| `name`、`version` | 必填。点分数字版本（`1.2.3`，可带 `v` 前缀，忽略 `-后缀`）按数值比较以检测市场更新；其他字符串按是否相等比较 |
| `min_resin_version` | 可选的点分版本号。要求更新版本 Resin 的插件包会在安装/加载时被拒绝。开发构建（`dev`）跳过此检查 |
| `capabilities.request_hook` | 接收 `request.inspect` 调用 |
| `capabilities.events` | 订阅的事件：精确类型、`*`，或 `lease.*` 这样的 `前缀.*` |
| `config_fields` | 配置对象顶层键的 schema，WebUI 据此渲染表单。未声明的键原样透传 |
| `runtimes` | `<goos>-<goarch>`（如 `linux-arm64`、`windows-amd64`）或 `any`。相对路径的 `command[0]`（包含 `/` 或 `\`，或以 `.` 开头）在插件包内解析，且不能逃出插件包（Windows 上当文件没有扩展名且存在对应 `.exe` 时自动补 `.exe`）；绝对路径原样使用；`python3` 这样的裸命令名在 `PATH` 中查找。工作目录为插件包目录 |

配置字段类型：

| 类型 | JSON 值 | WebUI 控件 |
| --- | --- | --- |
| `string` | 字符串 | 文本输入框 |
| `secret` | 字符串 | 密码输入框 |
| `text` | 字符串 | 多行文本框 |
| `number` | 数字 | 数字输入框 |
| `integer` | 整数 | 数字输入框 |
| `boolean` | 布尔值 | 开关 |
| `enum` | `enum_values` 之一 | 下拉选择 |
| `string_list` | 字符串数组 | 多行文本框，每行一项 |
| `json` | 任意 JSON | JSON 编辑器 |

### 协议

Resin 启动插件进程，并与之交换 [JSON-RPC 2.0](https://www.jsonrpc.org/specification) 消息，**每行一个 JSON 对象**（NDJSON）：宿主 → 插件走 stdin，插件 → 宿主走 stdout。写到 stderr 的内容会被复制到 Resin 日志。stdout 上不要输出任何其他内容。

请求可能交错：`request.inspect` 调用会并发发送，响应可以按任意顺序返回（按 `id` 匹配）。

| 方法 | 参数 | 结果 | 说明 |
| --- | --- | --- | --- |
| `plugin.register` | `{schema_version, resin_version, plugin_id, data_dir, config}` | `{schema_version: 1, name?, version?, capabilities?}` | 第一个调用。回复前先应用 `config`；返回错误表示拒绝启动。`capabilities` 只能收窄（不能扩大）清单声明的能力 |
| `plugin.configure` | `{config}` | `{}` | 热更新配置。出错时保留旧配置，管理员会看到错误信息 |
| `request.inspect` | `RequestInfo` | `RequestDecision` | 仅在声明 `request_hook` 时调用。必须在管理员配置的超时内回复 |
| `event.batch` | `{events: [{type, time, data}]}` | `{}` | 批次按顺序逐个发送 |
| `plugin.shutdown` | `{}` | `{}` | 释放资源并退出。之后 stdin 会被关闭，若 3 秒内未退出则强制结束进程 |

`RequestInfo`：

```json
{
  "proxy_type": "forward | reverse | socks5",
  "is_connect": false,
  "client_ip": "10.0.0.8",
  "platform": "openai",
  "account": "user_tom",
  "target_host": "api.example.com",
  "method": "POST",
  "url": "https://api.example.com/v1/chat/completions",
  "headers": {"User-Agent": ["curl/8.0"]}
}
```

`RequestDecision`（所有字段可选；`{}` 表示不做修改继续）：

```json
{
  "action": "continue | reject",
  "status": 429,
  "message": "Rate limit exceeded",
  "platform": "openai-backup",
  "account": "user_tom",
  "set_headers": {"X-Tenant": "tom"},
  "remove_headers": ["X-Debug"]
}
```

完整交互示例：

```
→ {"jsonrpc":"2.0","id":1,"method":"plugin.register","params":{"schema_version":1,"resin_version":"1.2.0","plugin_id":"example.rate-limit","data_dir":"/var/lib/resin/plugins/.data/example.rate-limit","config":{"rate_per_second":10}}}
← {"jsonrpc":"2.0","id":1,"result":{"schema_version":1}}
→ {"jsonrpc":"2.0","id":2,"method":"request.inspect","params":{"proxy_type":"reverse","platform":"openai","account":"tom","target_host":"api.example.com"}}
← {"jsonrpc":"2.0","id":2,"result":{"action":"reject","status":429,"message":"Rate limit exceeded"}}
→ {"jsonrpc":"2.0","id":3,"method":"plugin.shutdown","params":{}}
← {"jsonrpc":"2.0","id":3,"result":{}}
```

如果进程意外退出，Resin 会以指数退避（1 秒到 30 秒）重启它，并用当前配置重新发送 `plugin.register`。插件重启期间的调用会立即失败，并按 `fail_closed` 处理。

### Go SDK

Go 插件可以使用 `github.com/Resinat/Resin/pkg/pluginsdk`，它只依赖标准库：

```go
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

type blocker struct{ host string }

func (b *blocker) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg struct{ Host string `json:"host"` }
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	b.host = cfg.Host
	return nil
}

func (b *blocker) InspectRequest(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	if req.TargetHost == b.host {
		return pluginsdk.Reject(403, "blocked"), nil
	}
	return nil, nil // 继续
}

func main() {
	if err := pluginsdk.Serve(&blocker{}); err != nil {
		log.Fatal(err)
	}
}
```

实现 `pluginsdk.RequestInspector` 以处理请求钩子，实现 `pluginsdk.EventHandler` 以接收事件，还可以选择实现 `pluginsdk.Initializer`（接收包含 `DataDir` 的 `RegisterParams`）和 `pluginsdk.Shutdowner`。`InspectRequest` 会被并发调用。使用 `CGO_ENABLED=0` 构建可得到自包含的二进制。

完整示例见 [`examples/plugins`](../examples/plugins)：一个 Go 限流插件和一个无第三方依赖的 Python 事件导出插件。

## 安装

- **上传**：WebUI → 插件 → *上传插件*，或 `POST /api/v1/plugins/actions/upload`，请求体为压缩包原始内容。
- **目录**：将插件包复制到 `$RESIN_PLUGIN_DIR/<id>/`，然后点击 *重新扫描*（`POST /api/v1/plugins/actions/rescan`）。重新扫描还会重启 `plugin.json` 有变化的插件，并移除已删除的插件。
- **插件市场**：WebUI → 插件 → *插件市场* → *安装*。

为已安装的插件安装新版本时，会替换其文件，但保留设置和数据目录。如果插件已启用且新版本启动失败，会恢复旧版本。卸载会删除插件包、数据目录和设置。

## 插件市场索引

插件市场是任何提供索引文档的 HTTP(S) URL。可以配置多个索引；多个索引列出同一 id 时，以先配置的 URL 为准。

```json
{
  "schema_version": 1,
  "name": "My plugins",
  "plugins": [
    {
      "id": "example.rate-limit",
      "name": "Rate Limit",
      "version": "1.0.0",
      "description": "Token-bucket rate limit per account.",
      "author": "you",
      "homepage": "https://example.com",
      "license": "MIT",
      "tags": ["security"],
      "min_resin_version": "1.0.0",
      "artifacts": [
        {"os": "linux", "arch": "amd64", "url": "example.rate-limit-1.0.0-linux-amd64.tar.gz", "sha256": "<hex>", "size": 1234567},
        {"os": "any", "arch": "any", "url": "https://cdn.example.com/example.rate-limit-1.0.0.zip", "sha256": "<hex>"}
      ]
    }
  ]
}
```

- 制品 URL 可以是相对于索引 URL 的相对路径，因此把 `index.json` 与压缩包放在一起的 GitHub Pages 站点或对象存储桶即可作为插件市场。
- 会为当前平台选择最合适的制品：先精确匹配 `os`/`arch`，其次是 `os` 匹配且 `arch: "any"`，最后是 `any`/`any`。
- `sha256` 必填，并在解压前校验。压缩包中 `plugin.json` 的 id 必须与索引中的 id 一致。
- `min_resin_version` 高于当前 Resin 版本的条目会被列出，但不可安装。

参见 [`examples/plugins/index.example.json`](../examples/plugins/index.example.json)。

## HTTP API

所有端点都需要管理令牌。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/v1/plugins` | 列出插件（分页：`limit`、`offset`） |
| `GET` | `/api/v1/plugins/{id}` | 获取单个插件 |
| `PATCH` | `/api/v1/plugins/{id}` | 更新 `enabled`、`priority`、`timeout_ms`、`fail_closed`、`config`（`config` 整体替换） |
| `DELETE` | `/api/v1/plugins/{id}` | 卸载包插件（内置插件返回 `409`） |
| `POST` | `/api/v1/plugins/actions/rescan` | 重新扫描插件目录 |
| `POST` | `/api/v1/plugins/actions/upload` | 安装以请求体发送的 `.zip` / `.tar.gz`（最大 100 MiB） |
| `GET` | `/api/v1/plugin-marketplace` | 列出所有插件市场索引中的插件及其安装状态 |
| `POST` | `/api/v1/plugin-marketplace/{id}/actions/install` | 安装或升级插件市场中的插件 |

未启用外部插件时，上传和插件市场端点返回 `409 CONFLICT`；此时包插件完全不会被加载，因此对它们的 `GET`/`PATCH`/`DELETE` 返回 `404`。
