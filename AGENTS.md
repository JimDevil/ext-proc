# AGENTS.md

## 项目概览

- 用途：`ext-proc` —— Envoy ext_proc（External Processing）外置处理器，承接 Higress WASM 插件业务的迁移（gRPC 数据面服务，非 HTTP API）
- 已迁移插件：`model-router`（模型路由头注入/自动路由）、`apikey-extract`（API Key 多源提取与注入）
- 技术栈：自有 SDK（`sdk/` 包，fork 自 envoy-extproc-sdk-go 并重构，见 sdk/README.md）+ GoFrame v2（配置加载/热更新/错误处理）+ `log/slog`（运行时日志）
- Go 版本：1.26（以 go.mod 为准；不得使用已废弃 API，如 io/ioutil）
- 模块路径：`github.com/JimDevil/ext-proc`（独立开源仓库；`sdk/` 是对外的框架包，`internal/` 仍禁止外部 import）

## 架构约定（改代码前必读）

- **filter chain**：`internal/plugin` 定义插件接入标准（`Plugin` 接口）、注册表与 `Chain`；`internal/processor` 把 SDK 的六阶段回调桥接到 filter chain；`internal/server` 负责进程装配
- **每个插件一个包**（`internal/plugins/<name>`），插件之间零依赖，只依赖 `internal/plugin` 暴露的接口与 SDK 的纯数据类型
- **配置驱动**：每个插件在 `manifest/config/config.yaml` 的 `chain.plugins` 下独立分段，`enabled` 单独开关、`order` 决定 filter chain 顺序、`config` 为插件私有配置（由插件自己的 `Init` 解析）
- **热更新**：gcfg watch 触发后整体重建 filter chain 并原子替换，在途请求继续用旧链直至结束——插件实例视为不可变，**不要在插件实例上做运行时可变状态**（跨请求状态请放注册表并加锁）

## 命令（改动后必须全部通过）

- 构建：`go build ./...`
- 静态检查：`go vet ./...`（golangci-lint 可用时加跑 `golangci-lint run`）
- 测试：`go test -race -count=1 ./...`
- 格式化：`gofmt -l .` 必须无输出
- 依赖变更后：`go mod tidy && go mod vendor`（仓库使用 vendor，内网可离线构建）

## 代码规范

- 错误处理：用 `gerror.Wrap` 包装保留链路（gerror 兼容 `errors.Is/As`）；判断用 `errors.Is/As`；禁止 `_ = err` 和 panic 传业务错误
- context：作为函数第一个参数显式传递；禁止存入 struct 字段
- 接口：在消费方定义小接口（插件依赖 `plugin.Context` 窄接口，不直接依赖 SDK 的 RequestContext）；避免 `interface{}`（跨插件 KV 共享态除外，值类型自管）
- 并发：每条流一个 goroutine，插件回调内**禁止另起 goroutine**（流取消时无法通知，会泄漏）；包级跨请求状态必须加锁
- 日志：`log/slog` 结构化日志，不使用 fmt.Println；日志消息一律英文
- 包名：小写、单数、无下划线；避免 util/helpers 这类无信息包名
- 注释：一律英文（国际化开源项目），说明"为什么"而不是"做什么"

## 插件开发规范（本仓库特有）

1. 在 `internal/plugins/<name>` 新建包，实现 `plugin.Plugin`（嵌入 `plugin.BasePlugin`，仅覆写关心的阶段）
2. 私有配置解析放本包 `Init(cfg *gvar.Var, logger)`，**默认值在本包内给全**；`cfg` 可能为 nil（配置段缺省），必须处理
3. `init()` 里调用 `plugin.Register("<name>", New)` 完成自注册
4. `manifest/config/config.yaml` 的 `chain.plugins` 增加配置段（含 enabled/order/config）
5. 短路（拒答/重定向）统一走 `ctx.CancelRequest`；后续阶段若依赖 headers 阶段的信息，用 `ctx.SetValue/GetValue` 传递
6. 插件自身故障**不要静默吞错**：返回 error，由 `chain.failurePolicy`（fail_closed/fail_open）统一决定放行或中断
7. 新增/修改插件必须带表驱动测试（mock 用法见 `internal/plugin/plugintest` 与现有插件测试）

## 测试要求

- 表驱动测试（t.Run + t.Parallel 酌情）；新增/修改逻辑必须带测试
- 禁止为通过测试而修改断言；测试失败先怀疑实现

## 边界

- `internal/` 下的包不得被外部 import；生成代码（*.pb.go 等）禁止手改
- 第三方依赖白名单：`gogf/gf/v2`（配置/错误，任务指定）、`tidwall/gjson+sjson`（对齐 higress model-router 原实现的 body 解析方式）、`envoyproxy/go-control-plane`（ext_proc 协议生成类型，跟随 Envoy 官方演进）、`google.golang.org/grpc` + `prometheus/client_golang`（sdk/ 传递依赖）；ext_proc 服务端 SDK 已内化为仓库内 `sdk/` 包，不再引用外部实现；新增依赖必须先在会话中说明理由获得确认
- 改动保持最小，不顺手重构无关代码
- 安全：凭据只从环境变量或密钥服务读取；源码、示例和测试都不得写入可用的凭据字面量
