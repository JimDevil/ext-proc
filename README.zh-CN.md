# ext-proc

[English](README.md) | **简体中文**

把网关插件从 WASM 迁到进程外 —— 基于 [Envoy](https://www.envoyproxy.io) [ext_proc](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter) filter 的责任链插件框架。

每个插件就是一个标准 gRPC 服务上的一个 Go 包：独立部署、独立扩缩容、崩溃不波及 Envoy；generation 安全的热更新、插件级失败策略、TLS/mTLS、Prometheus 指标、W3C trace 传播，以及开箱即用的两个 LLM 网关插件。

> **💡 把它当脚手架用。** 这个仓库就是开发 ext_proc filter 的现成起点：克隆后删掉两个示例插件（`internal/plugins/` 下的包、`main.go` 里的导入、`config.yaml` 里的配置段——或者直接 `enabled: false`），然后在 `internal/plugins/<your-plugin>` 下开发自己的业务。数据面服务需要的一切——责任链、热更新、指标、优雅退出、panic 防护——全部由框架层兜底。两个示例插件同时是可运行、可研究、可移除的参考实现。见[写一个自己的插件](#写一个自己的插件)。

## 为什么离开 WASM

WASM 插件与 Envoy 同进程共生——内存、故障、运维全部耦合。ext_proc 把插件逻辑放进一个普通进程，代价只是每个处理阶段一次 gRPC 往返。

| 维度 | WASM 插件 | ext-proc |
|---|---|---|
| 内存与尾延迟 | VM 200MB 重建阈值：重建窗口丢 header、阻塞 worker，冷却期引发重建风暴 | **没有 VM：不存在重建窗口**，配置热更新原子换链 |
| 故障域 | 插件 panic 直接掐流（`503 wasm_fail_stream`），FAIL_OPEN 静默放行 | 进程隔离：插件 panic 按流恢复，进程崩溃也不会带崩 Envoy；**fail-open / fail-closed 显式可配**，支持插件级回滚 |
| 语言与调试 | TinyGo 限制多，wasm log 黑盒度高 | 任意 gRPC 语言；标准 k8s 工具链，可观测、可断点调试 |
| 部署运维 | 内嵌 Envoy 进程，随网关一起发布 | 独立服务：HPA / PDB / 优雅退出 / 灰度发布全部现成 |
| 流式处理 | HttpBody 逐 chunk 回调，大 body 解析易 panic | SDK 支持 chunked / `FULL_DUPLEX_STREAMED`；示例插件采用 BUFFERED 语义并跳过不完整分块 |

## 架构

![ext-proc 生产架构：Envoy 请求路径、六阶段责任链、generation 生命周期、两种部署形态与可观测性](docs/assets/chain-architecture.svg)

**① 请求路径。** 请求进入 Envoy，`ext_proc` HTTP filter 为每个请求打开一条双向 gRPC 流；Envoy 依次发送最多六个阶段（request headers/body/trailers、response headers/body/trailers），SDK 返回 header/body mutation，或用 `ImmediateResponse` 短路上游调用。每条流由一个 goroutine 处理，mutation 按阶段累积。

**② ext-proc 内部。** `RequestContext` 持有流状态（请求元数据、跨插件 KV、解析后的 W3C `traceparent`）。`RouteTable` 是不可变 generation：每个启用插件解析首条命中的 `matchRule`（或回退全局配置），`Chain` 再按 `order` 执行插件实例。责任链负责插件级 `failurePolicy`、`fail_open` 时的 mutation/请求态回滚、插件边界 panic 恢复，以及 `plugin`/`phase` 指标。插件实例不可变，`Close` 必须幂等。

**③ Generation 与运维。** 配置热更新先完整构建并校验下一代，再原子发布。在途流 pin 住自己的 generation；旧 generation 在最后一个流结束后 `Close`，插件持有的连接/缓存可以安全释放且不影响在线流量。构建失败保留上一代。同一个二进制既支持 loopback sidecar，也支持带 TLS/mTLS + NetworkPolicy 的跨 Pod Deployment；`/metrics`、`/healthz`、`/readyz` 覆盖观测与摘流。

每请求一条流 · 一流一个 goroutine · `ImmediateResponse` 短路即终止 · fail-open / fail-closed 双层容错。

## 核心特性

- **责任链编排** —— 按配置 `order` 升序执行，任一插件短路即停止后续插件；插件之间零依赖，只共享请求级状态。
- **标准化插件接入** —— 六阶段回调 + `BasePlugin` 空实现 + `init()` 自注册：一个包、五步接入、互不影响。
- **配置热更新** —— GoFrame gcfg watch `config.yaml`，责任链原子重建；在途请求继续走旧链，旧 generation 在最后一个流结束后 `Close`，全程零重启。
- **双层容错** —— 链级 `fail_open` / `fail_closed` 与 Envoy `failure_mode_allow` 对齐，且每个插件可单独覆盖（`plugins[].failurePolicy`）：同一条链里鉴权插件 fail-closed、路由辅助插件 fail-open。`fail_open` 会回滚该插件的 mutation 与请求态，并计入插件失败指标。
- **可观测性** —— Prometheus 指标（流数、阶段错误、带经典 bucket 的耗时直方图，以及按 `plugin`/`phase` 拆分的插件延迟与失败数）+ gRPC/HTTP 健康检查 + 只记录元数据的 slog 结构化日志。原始 header 和 body 永不落日志。指标目录见 [`docs/metrics.md`](docs/metrics.md)。Grafana dashboard：[`manifest/deploy/grafana-dashboard.json`](manifest/deploy/grafana-dashboard.json)。
- **生产工程** —— 三步曲优雅退出（摘流 → 传播延迟 → 有界排空）；gRPC 流控交给 Envoy 背压；**panic 防护** —— 插件或处理器 panic 只影响当前流，按 failurePolicy 处理并计入 `extproc_panic_recoveries_total`。

## 生产化能力

| 维度 | 本仓库提供的能力 |
|---|---|
| 数据面 | 每请求一条 gRPC 流、六阶段回调、BUFFERED/STREAMED 防护、可配置 gRPC 消息上限、有界 body 处理 |
| 失败语义 | 插件级 `fail_open` / `fail_closed`、插件边界 panic 恢复、mutation + 请求态回滚、`ImmediateResponse` 无条件拒绝 |
| 生命周期 | 事务性热更新（构建 → 校验 → 原子发布）、按流 pin generation、最后一个流结束后 `Close` 旧代、插件 `Close` 幂等 |
| 安全 | header 名/值校验、Envoy 受保护头拒绝、元数据日志不含凭据、TLS/mTLS、证书/CA 热加载、NetworkPolicy 示例 |
| 可观测性 | stream/phase/plugin Prometheus 指标、经典直方图（stream duration 另带 native）、W3C `traceparent` 传播、元数据日志、`/healthz` `/readyz`、gRPC health Watch、Grafana dashboard |
| 交付 | vendor 离线构建、scratch 多架构镜像、版本/commit 标签、Docker healthcheck、CI（build/vet/race ×2）、Envoy e2e 与 k6 压测 |

## 运维端点

| 端点 | 用途 |
|---|---|
| gRPC `:50051` | `envoy.service.ext_proc.v3.ExternalProcessor` |
| gRPC health | `grpc.health.v1.Health/Check` 与 `Watch`；摘流时转为 NOT_SERVING |
| HTTP `:19090/metrics` | Prometheus 指标（默认端口，可配置） |
| HTTP `:19090/healthz` | 存活探针；进程运行即 200 |
| HTTP `:19090/readyz` | 就绪探针；SIGTERM 开始 drain 后返回 503 |
| `ext-proc --healthcheck` | scratch 镜像/Docker healthcheck 辅助命令 |

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
./bin/ext-proc   # gRPC :50051 · Prometheus :19090/metrics
# 或：make run
# EXT_PROC_CONFIG=/path/to/config.yaml 可覆盖配置文件路径
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

多阶段 `Dockerfile`：从 vendor 依赖离线构建静态二进制，scratch 运行时带 CA 证书、非 root 运行，注入版本/commit/构建时间。多架构就绪（感知 `TARGETARCH`）：

```shell
make image          # 构建本机架构镜像并冒烟运行容器
make image-multi    # 构建并推送 linux/amd64 + linux/arm64（需 buildx + 登录 registry）
```

scratch 镜像支持 Docker/Kubernetes exec 健康检查命令：

```shell
/app/ext-proc --healthcheck   # GET http://127.0.0.1:19090/healthz
```

### 端到端与压测

compose 栈会启动 Envoy → ext_proc → echo upstream：

```shell
make e2e          # 启动栈、断言注入头、自动停止
make loadtest     # 对运行中的栈做 k6 压测（需 k6）
```

细节和阈值见 [`test/e2e/README.md`](test/e2e/README.md)。

## 部署形态

同一个二进制同时支持两种部署方式，区别在监听地址和安全边界：

| | 同 Pod sidecar | 跨 Pod Deployment |
|---|---|---|
| `server.bindAddress` | `127.0.0.1` | `0.0.0.0`（配合 NetworkPolicy） |
| Envoy cluster | `127.0.0.1:50051`（h2c） | `ext-proc.<ns>.svc:50051` |
| TLS | 回环无需 TLS；在网关终止 | 开启 `server.tls`；mTLS 配置 `clientCAFile` |
| 扩缩容 | 跟随网关 Pod | 独立副本 + PDB/HPA |
| 示例 | `manifest/deploy/sidecar/` | `manifest/deploy/deployment.yaml` |

```yaml
server:
  bindAddress: 127.0.0.1          # sidecar 形态
  metricsBindAddress: 127.0.0.1
  maxRecvMsgSize: 16777216        # 与 Envoy buffer 上限对齐
  maxSendMsgSize: 16777216
  tls:
    enabled: false                # 跨 Pod 改为 true，并配置 certFile/keyFile/clientCAFile
    # reloadIntervalSeconds: 30   # 轮询证书/CA 文件实现轮换
```

跨 Pod 部署必须开启 TLS/mTLS、挂载证书 Secret，并用 NetworkPolicy 只允许网关和监控命名空间访问。两种形态的完整清单见 [`manifest/deploy/`](manifest/deploy/)。

## 写一个自己的插件

插件之间零依赖，不同的插件可以由不同的人并行开发：

1. **新建包** `internal/plugins/<name>`，嵌入 `plugin.BasePlugin`，只覆写关心的阶段。
2. **实现接口** —— 六阶段回调按需实现；`Init` 解析自己的配置段并给全默认值。
3. **自注册** —— `init()` 里 `plugin.Register`，并在 `main.go` 追加一行匿名导入。
4. **配置加段** —— `config.yaml` 的 `chain.plugins` 增加 `name/enabled/order/config`。省略 `enabled` 表示启用；要关闭请显式写 `enabled: false`。
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

- **失败策略**：`fail_closed` 返回 gRPC 错误，最终是否放行仍由 Envoy 的 `failure_mode_allow` 决定。必须无条件拒绝的请求（例如鉴权）应使用 `CancelRequest` 发本地响应，`apikey-extract` 的 required 模式就是这么做的。
- **热更新**：只有 `chain` 段可热更新。新配置会先完整构建并校验，成功后才原子发布；构建失败保留旧链和旧配置。`app` / `logger` / `server` 变更需要重启进程。旧 generation 在最后一个在途流结束后关闭，因此插件可以持有连接/缓存，只要 `Close` 幂等释放。
- **Envoy 受保护头**：`:method`、`:authority`、`host`、`x-envoy-*` 不可改写；SDK 会在写入前直接返回明确错误。
- **Content-Length**：插件改写 body 时由 Envoy 重新计算，SDK 不再写入；插件不应自己设置 `content-length`。
- **大 body**：`server.maxRecvMsgSize` / `maxSendMsgSize` 必须不小于 Envoy buffer 的 body 上限。`BUFFERED` 下一条消息就是完整 body；`STREAMED` 下样例插件会识别分块并跳过，避免改写不完整的 JSON。
- **TLS 轮换**：设置 `server.tls.reloadIntervalSeconds`（例如 `30`）轮询证书/CA 文件；加载失败保留旧证书材料，`0` 表示关闭轮询、需重启。
- **链路追踪**：SDK 解析 W3C `traceparent`，在元数据日志和 `plugin.Context.RequestInfo()` 中提供 `trace_id`/`span_id`，不引入额外 exporter。
- **插件上下文**：下游调用/取消用 `ctx.Context()`，请求元数据（method/path/request_id/trace_id）用 `ctx.RequestInfo()`；修改影响路由的 header 后调用 `ctx.ClearRouteCache()`。
- **日志**：运行时日志只记录元数据（phase、request id、method、path、body 长度）。生产环境不要开启原始 header/body 日志；样例默认级别为 `info`。
- **SDK**：ext_proc 服务端 SDK 内置于 [`sdk/`](sdk/)（fork 自 [wrossmorrow/envoy-extproc-sdk-go](https://github.com/wrossmorrow/envoy-extproc-sdk-go) 并重构，随本仓库维护——差异见其 README）。

## 安全

漏洞披露流程与部署/安全不变量见 [SECURITY.md](SECURITY.md)。

## 许可证

Apache-2.0
