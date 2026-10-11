# ext-proc

[English](README.md) | **简体中文**

把网关插件从 WASM 迁到进程外 —— 基于 [Envoy](https://www.envoyproxy.io) [ext_proc](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter) filter 的 filter chain 插件框架。

每个插件就是一个标准 gRPC 服务上的一个 Go 包：独立部署、独立扩缩容、崩溃不波及 Envoy；配置热更新、filter chain 编排，开箱即用两个 LLM 网关插件。

> **💡 把它当脚手架用。** 这个仓库就是开发 ext_proc filter 的现成起点：克隆后删掉两个示例插件（`internal/plugins/` 下的包、`main.go` 里的导入、`config.yaml` 里的配置段——或者直接 `enabled: false`），然后在 `internal/plugins/<your-plugin>` 下开发自己的业务。数据面服务需要的一切——filter chain、热更新、指标、优雅退出、panic 防护——全部由框架层兜底。两个示例插件同时是可运行、可研究、可移除的参考实现。见[写一个自己的插件](#写一个自己的插件)。

## 为什么离开 WASM

WASM 插件与 Envoy 同进程共生——内存、故障、运维全部耦合。ext_proc 把插件逻辑放进一个普通进程，代价只是每个处理阶段一次 gRPC 往返。

| 维度 | WASM 插件 | ext-proc |
|---|---|---|
| 内存与尾延迟 | VM 200MB 重建阈值：重建窗口丢 header、阻塞 worker，冷却期引发重建风暴 | **没有 VM：不存在重建窗口**，配置热更新原子换链 |
| 故障域 | 插件 panic 直接掐流（`503 wasm_fail_stream`），FAIL_OPEN 静默放行 | 进程隔离：崩溃只影响在途流，**fail-open / fail-closed 显式可配** |
| 语言与调试 | TinyGo 限制多，wasm log 黑盒度高 | 任意 gRPC 语言；标准 k8s 工具链，可观测、可断点调试 |
| 部署运维 | 内嵌 Envoy 进程，随网关一起发布 | 独立服务：HPA / PDB / 优雅退出 / 灰度发布全部现成 |
| 流式处理 | HttpBody 逐 chunk 回调，大 body 解析易 panic | `FULL_DUPLEX_STREAMED` 模式天然适配 LLM SSE 透传 |

## 架构

```
 Client                      Envoy Gateway                          ext-proc
┌───────┐   request    ┌──────────────────────┐   bidirectional  ┌───────────────────────┐
│ curl/ │ ──────────▶  │  envoy.filters.http  │ ──────gRPC─────▶ │  filter-chain         │
│ SDK   │              │  .ext_proc           │ ◀──────────────  │  ├─ model-router      │
└───────┘              │  (5 phases: headers, │    mutations     │  └─ apikey-extract    │
                       │   body, trailers)    │                  │  (+ your plugins)     │
                       └──────────┬───────────┘                  └───────────────────────┘
                                  │ route / forward (with injected headers)
                                  ▼
                              Upstream
```

每请求一条流 · 一流一个 goroutine · `ImmediateResponse` 短路即终止 · fail-open / fail-closed 双层容错。

![filter chain——matchRules 按请求组装、插件级 failurePolicy、短路传播、原子热交换](docs/assets/chain-architecture.svg)

## 核心特性

- **filter chain 编排** —— 按配置 `order` 升序执行，任一插件短路即停止后续插件；插件之间零依赖，只共享请求级状态。
- **标准化插件接入** —— 六阶段回调 + `BasePlugin` 空实现 + `init()` 自注册：一个包、五步接入、互不影响。
- **配置热更新** —— GoFrame gcfg watch `config.yaml`，filter chain 原子重建；在途请求继续走旧链，全程零重启。
- **双层容错** —— 链级 `fail_open` / `fail_closed` 与 Envoy `failure_mode_allow` 对齐，且每个插件可单独覆盖（`plugins[].failurePolicy`）：同一条链里鉴权插件 fail-closed、路由辅助插件 fail-open。静默放行必有指标可查、可告警。
- **可观测性** —— Prometheus 指标（流数、阶段错误、耗时直方图）+ gRPC 健康检查 + slog 结构化日志；Envoy 侧每阶段延迟可通过 filter state 进 access log。
- **生产工程** —— 三步曲优雅退出（摘流 → 传播延迟 → 有界排空）；gRPC 流控交给 Envoy 背压；**panic 防护** —— 处理器 panic 只终结当前流，由 `extproc_panic_recoveries_total` 计数。

## 示例插件

### `model-router` *（迁移自 higress model-router 插件）*

从请求体提取模型名并注入路由头，是模型路由 / 多 provider 网关的地基插件。

- **JSON / multipart**：gjson 按需提取，不整包解析；multipart 表单字段同样支持
- **`provider/model` 前缀拆分**：provider 注入独立头，body 模型名可选去前缀（`keepOriginalModelName`）
- **`higress/auto` 自动路由**：正则规则匹配最后一条 user 消息，未命中回落 `defaultModel`

### `apikey-extract`

把散落在各处的 API Key 归一到目标请求头，供后续插件与上游统一消费。

- **多来源有序回退**：`x-api-key` 头 → `Authorization: Bearer` → `apikey` query 参数
- **防泄漏**：`stripSource` 删除来源头（query 来源协议上不可删）
- **required 模式**：所有来源未命中时 401 拒绝，响应体 JSON 化防注入

## 路由级配置（matchRules）

与 higress wasmPlugin 的 `matchRules` 对齐——但**由每个插件独立持有**：每个插件自带规则、按请求独立解析（一个插件走规则覆盖而另一个保持全局，是常态）。规则按顺序求值——**首条命中生效**——未命中的请求使用该插件的全局配置：

```yaml
plugins:
- name: model-router
  order: 10
  config: { modelToHeader: x-higress-llm-model }   # 全局默认
  matchRules:
  - domain: ["api.example.com"]
    pathPrefix: /v1/chat
    config: { modelToHeader: x-model-chat }        # 深合并覆盖全局配置
  - domain: ["*.internal.corp"]
    disable: true                                  # 该域名下关闭此插件
- name: apikey-extract
  order: 20
  matchRules:
  - pathPrefix: /internal/
    disable: true
```

- `domain` 支持精确、`*.通配` 或 `*`（匹配前剥离端口、大小写不敏感）；`pathPrefix` 为前缀匹配——一条规则的所有维度都命中才算命中。
- 规则的 `config` 与插件全局配置深合并；`disable: true` 在命中路由关闭该插件（不影响其他插件）。
- 每个插件的变体在配置加载时预构建（每规则一个实例），整表热更新原子换——在途请求走旧表，新请求立即使用命中的变体。

## 快速开始

```shell
# 1. 构建并运行（依赖已 vendor，离线可构建）
git clone https://github.com/JimDevil/ext-proc.git && cd ext-proc
make vendor && make build
./ext-proc   # gRPC :50051 · Prometheus :19090/metrics
```

接入 Envoy（关键配置——Istio/Higress 的 `EnvoyFilter` 形式见 `manifest/envoyfilter-demo.yaml`）：

```yaml
http_filters:
- name: envoy.filters.http.ext_proc
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
    stat_prefix: ext_proc
    grpc_service:
      envoy_grpc: { cluster_name: ext-proc-cluster }
    processing_mode:
      request_header_mode: SEND
      request_body_mode: BUFFERED      # 插件拿完整 JSON/multipart
      response_header_mode: SEND
      response_body_mode: NONE
    message_timeout: 2s
    failure_mode_allow: false          # 鉴权/路由类插件建议 fail-closed
```

```shell
# 2. 发一个请求，观察插件注入的头
curl -s http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "x-api-key: your-key" \
  -d '{"model":"glm-5.3","messages":[{}]}'

# 上游收到的请求头（echo 服务 / 网关 access log 可见）：
#   x-higress-llm-model: glm-5.3      ← model-router 注入
#   x-extracted-apikey:  your-key     ← apikey-extract 注入
```

### Docker

多阶段 `Dockerfile`：从 vendor 依赖离线构建静态二进制，scratch 运行时带 CA 证书、非 root 运行，镜像约 10MB。多架构就绪（感知 `TARGETARCH`）：

```shell
make image          # 构建本机架构镜像并冒烟运行容器
make image-multi    # 构建并推送 linux/amd64 + linux/arm64（需 buildx + 登录 registry）
```

## 写一个自己的插件

插件之间零依赖，不同的插件可以由不同的人并行开发：

1. **新建包** `internal/plugins/<name>`，嵌入 `plugin.BasePlugin`，只覆写关心的阶段。
2. **实现接口** —— 六阶段回调按需实现；`Init` 解析自己的配置段并给全默认值。
3. **自注册** —— `init()` 里 `plugin.Register`，并在 `main.go` 追加一行匿名导入。
4. **配置加段** —— `config.yaml` 的 `chain.plugins` 增加 `name/enabled/order/config`。
5. **表驱动测试** —— 用 `plugintest` 的 mock Context 断言 header/body 操作，`make test` 全绿。

```go
package myplugin

type MyPlugin struct {
	plugin.BasePlugin // 只覆写需要的阶段
}

func New() plugin.Plugin { return &MyPlugin{} }
func init()              { plugin.Register("my-plugin", New) }

func (m *MyPlugin) Name() string { return "my-plugin" }

func (m *MyPlugin) OnRequestHeaders(ctx plugin.Context, h extproc.AllHeaders) error {
	// 短路：本地响应终止请求；放行：返回 nil 即可
	if _, blocked := h.Get("x-block"); blocked {
		return ctx.CancelRequest(403, nil, "blocked by my-plugin")
	}
	return nil
}
```

```yaml
chain:
  failurePolicy: fail_closed
  plugins:
    - name: model-router
      enabled: true
      order: 10
      config: { modelToHeader: x-higress-llm-model }
    - name: my-plugin
      enabled: true
      order: 20
      config: { yourKey: yourValue }   # 插件私有配置，由插件自己解析
```

## 注意事项

- **热更新**：修改 `config.yaml` 后 filter chain 自动重建（新请求走新链，在途请求走旧链）；插件实例视为不可变，不要在实例上存跨请求可变状态。
- **Envoy 受保护头**：`:method`、`:authority`、`host`、`x-envoy-*` 不可改写，Envoy 侧会拒绝。
- **Content-Length**：插件改写 body 时 Envoy 自动剥除，无需插件处理。
- **大 body**：`BUFFERED` 模式下受 Envoy `max_message_bytes` 限制；超大请求建议 `STREAMED` + 流式解析。当前两个插件均假设 BUFFERED 全量语义（与 higress 原实现对齐）。
- **SDK**：ext_proc 服务端 SDK 内置于 [`sdk/`](sdk/)（fork 自 [wrossmorrow/envoy-extproc-sdk-go](https://github.com/wrossmorrow/envoy-extproc-sdk-go) 并重构，随本仓库维护——差异见其 README）。

## 许可证

Apache-2.0
