# CLIProxyAPI Provider Rate Limiter

[English](README.md) | 简体中文

这是一个独立的 CLIProxyAPI 动态插件，通过 CPA 官方 `scheduler` 能力在账号选择前执行限流。它不需要 `X-Provider` 请求头，也不需要额外端口或路径。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    provider-rate-limiter:
      enabled: true
      priority: 100
      queue_enabled: true
      queue_max_wait_ms: 15000
      queue_max_waiters: 256
      default_rpm: 60
      providers:
        codex: 60
        claude: 30
        gemini: 60
        openai-compatible-deepseek: 30
      auths:
        codex-account-01: 120
        codex-account-02: 90
```

限流作用于 CPA 调度器暴露的每个授权候选，包括 Codex、Claude、Gemini、Antigravity、已配置的 API Key、OpenAI 兼容上游以及未来的 Provider 插件。每个 auth ID 拥有独立的滑动一分钟窗口；Provider 设置是每个候选各自的默认值，而不是整个 Provider 共享的配额。当插件返回一个 `AuthID` 时即计为一次准入，包括随后上游请求失败的情况。

优先级依次为：auth 覆盖、解析后的 Provider key、兼容短别名、兼容兜底，最后是全局默认值。对于 `openai-compatible-deepseek`，Provider keys 依次为 `openai-compatible-deepseek`、`deepseek`、`openai-compatibility`。兼容属性（`provider_key`，其次 `compat_name` 用于通用记录）用于识别逻辑上游；普通 Provider 类型保持不透明处理。显式配置 `0` 表示该层级不限流；未配置的覆盖则继承。负数限制会被拒绝。

## 排队与限流

默认启用排队。`queue_max_wait_ms` 默认为每次调度选择等待 15000 毫秒，`queue_max_waiters` 默认为每进程 256 个排队请求。`queue_enabled: false` 或 `queue_max_wait_ms: 0` 恢复立即拒绝。`queue_max_waiters: 0` 表示不限制排队数量，不建议在生产环境使用。

排队请求按到达顺序检查。没有空闲候选的排队请求会被跳过，因此无关的 Provider 池不会被阻塞；竞争同一候选集合的排队请求仍保持到达顺序。这是候选感知的 FIFO，而不是严格的全局 FIFO。只有成功准入才消耗配额。超时返回可重试的 HTTP 429 `provider_rate_limit_exceeded`，并附带 `waited_ms` 与 `next_free_in_ms`；队列已满返回 HTTP 429 `provider_rate_limit_queue_full`。排队用于吸收突发流量，而不是承接持续超过配置容量的流量，也不能保证消除 429。

重新配置会保留窗口记录并唤醒排队中的请求。降低等待上限可以缩短已有的截止时间，但提高上限不会延长已有等待。禁用排队会立即拒绝排队中的请求；降低队列容量只影响后续入队。CPA 可能会重试调度错误，因此总等待时间可能接近 `(request-retry + 1) * queue_max_wait_ms`，再加上其他请求处理时间。请结合调用方与代理超时时间进行调优。

当前 C ABI 不会把请求取消传递给插件。客户端断开后 CPA 可以立即返回，但插件的有界等待会继续，并可能在之后消耗一个配额。这不是完全取消感知的队列。Go 共享库在重载期间可能保持映射，因此内存中的历史可能在重新启用后保留；进程重启会清空它。不同 CPA 进程之间不共享配额或队列，即使加载同一个插件文件。不经过授权选择、直接由 model-router 执行器处理的请求会绕过调度器插件。

## 管理菜单

插件会向 CPA 注册 **Provider Rate Limiter** 管理菜单。打开侧边栏菜单并输入 CPA 管理密钥，即可加载合并后的账号列表。页面合并了 `/v0/management/auth-files` 与插件的 `/v0/management/plugins/provider-rate-limiter/candidates` 接口：在 auth-files 中没有条目、仅存在于配置中的授权，只有在被调度选择看到后才会出现。已观测候选列表是有界的进程内缓存（按精确 auth ID 最多 2000 条），不是权威的完整清单；页面会显示观测起始时间，并在列表不完整或加载失败时给出警告。行按精确 ID 合并，因此已观测候选可能引入 auth-files 中不存在的 Provider 名称；已保存覆盖对应的账号不存在时会标记为未匹配，仍可编辑或删除。每行显示当前生效的限制及其来源（auth 覆盖、Provider key 或全局默认值），全局卡片可编辑 `queue_enabled`、`queue_max_wait_ms` 与 `queue_max_waiters`。管理密钥只保留在当前页面中。保存时先向 `/v0/management/plugins/provider-rate-limiter/config` 发起 PATCH，再向运行时设置接口发起 PUT；如果配置已持久化但运行时保存失败，页面会明确提示这一区别，而不是笼统的成功。候选输出采用白名单：`config:` 来源类别与解析后的 Provider、仅保留源站的上游 URL、以及调度器身份字段——绝不包含令牌或原始属性。每个管理请求仍需 CPA 管理密钥；公开的菜单外壳本身不暴露任何账号数据。

## 构建

```bash
cd go
go test -race ./...
node --test menu_test.js
go vet ./...
go build -buildmode=c-shared -o /tmp/provider-rate-limiter.so .
```

将生成的动态库复制到目标架构对应的 CPA 插件目录（macOS 使用 `.dylib`），然后按照 CPA 的插件重载机制重启或重新加载。在当前 macOS 开发机上，cgo 构建需要 `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk`，因为所选的新版 SDK 与已安装的链接器不兼容；这只是本地工具链的临时方案，不是通用的必需默认值。窗口与排队状态保存在单个进程内存中；不同 CPA 进程之间不共享配额或队列。

## 本地 CPA 源码开发

如果需要使用尚未发布的 CPA SDK 变更，可以临时在 `go/go.mod` 添加：

```text
replace github.com/router-for-me/CLIProxyAPI/v7 => ../cliproxyapi-fork
```

发布版本前请删除这条 `replace`。

## GitHub Actions

仓库的 CI 会在 Linux 和 macOS 上执行 race 测试、静态检查和动态库构建。

## 自定义 CPA 插件商店源

本仓库是 [lsmallice 仓库](https://github.com/lsmallice/cliproxyapi-provider-rate-limiter-plugin) 的维护分叉，包含可作为第三方 CPA 插件商店源使用的 `registry.json`。在 CPA 配置的 `plugins.store-sources` 下添加：

**生产安装必须使用已打标签的 GitHub Release 归档，并核对其在 `checksums.txt` 中的 SHA-256；不要直接安装未经验证的分支构建。**

```yaml
plugins:
  store-sources:
    - https://raw.githubusercontent.com/hetonghao/cliproxyapi-provider-rate-limiter-plugin/ai-cove/main/registry.json
```
