# Claude CLI / Agent SDK 代理

## 接入

```sh
./build.sh
```

产物包括 `bin/codex` 和 `bin/claude`。Claude 固定读取当前 UID 的系统账户主目录下的 `.claude-proxy/env`，配置模板是 [claude.env.example](../claude.env.example)。构建脚本只创建 `env.example`，不创建或覆盖实际配置 `env`。

```dotenv
CLAUDE_PROXY_REAL_BIN=/absolute/path/to/native/claude
CLAUDE_PROXY_MAX_CONCURRENCY=1
CLAUDE_PROXY_WAIT_TIMEOUT=10m
CLAUDE_PROXY_LOG_MAX_SIZE_MIB=10
CLAUDE_PROXY_LOG_MAX_BACKUPS=5
```

真实路径必须是可执行的原生 ELF / Mach-O 文件，不接受 npm 的 JS 启动器、shell 脚本或代理自身。原生安装的 `claude` 或 SDK 平台包内的原生 `claude` 都可作为真实路径；使用绝对路径，不再次通过 `PATH` 查找。原始程序与代理要放在不同目录。

把 `bin/` 放在 `PATH` 前面即可拦截普通 CLI。ClaudeCodeUI 还应明确指定代理，以覆盖 SDK 使用包内二进制的情况：

```sh
export CLAUDE_CLI_PATH=/absolute/path/to/codex-proxy/bin/claude
export CLAUDE_CONFIG_DIR=/xxx/alice/.claude
```

环境变量应设置在启动 ClaudeCodeUI 服务之前。直接使用 SDK 时设置 `options.pathToClaudeCodeExecutable` 为 `bin/claude` 的绝对路径，并通过 `options.env` 保留调用方环境。SDK 直接 spawn 这个 Go 原生程序，不需要 Node 或 shell 包装。

`CLAUDE_CONFIG_DIR`、`HOME`、`USER`、工作目录和 Anthropic 认证环境原样传递给真实 CLI；它们不会改变代理配置、锁或日志的位置。同一个系统账户下的所有 Claude 代理共享额度，包括不同逻辑用户、不同项目以及不同运行时 `HOME`。Claude 的 `.claude-proxy` 与 Codex 的 `.codex-proxy` 分开，因此两种提供商的额度和提交时间互不影响。

配置格式、文件权限、动态调整上限和等待超时沿用 Codex 代理。每次获取槽位及等待重试都重读配置；已有任务不因降额被终止。等待超时只覆盖获取槽位，不包括获得槽位后的间隔等待或运行时间。改变真实二进制路径影响新进程，已有 stream 会话继续使用原来的子进程。

## ClaudeCodeUI 的实际调用链

分析依据是 [ClaudeCodeUI runtime](https://github.com/siteboon/claudecodeui/blob/main/server/modules/providers/list/claude/claude-runtime.provider.js)、[CLI 路径解析](https://github.com/siteboon/claudecodeui/blob/main/server/shared/claude-cli-path.ts)、[官方 Agent SDK](https://code.claude.com/docs/en/agent-sdk/overview) 和 [CLI 参数](https://code.claude.com/docs/en/cli-reference)。已用 ClaudeCodeUI 所声明的 TypeScript SDK **0.3.165** 实测协议兼容性，测试使用本地模拟 CLI，不调用模型。

SDK 会 spawn CLI，stdin/stdout 使用逐行 JSON。0.3.165 实际基础参数为：

```text
--output-format stream-json --verbose --input-format stream-json
```

该版本不额外传 `-p`。代理也接受带 `-p` / `--print` 的双向 stream-json 调用，保持其他参数与协议内容不变。stream 模式要求输入、输出均为 stream-json，提示词只从 stdin 提交；混用 argv 提示词或其他输出格式会拒绝，以免首次任务绕过逐条提交检查。

ClaudeCodeUI 每轮创建 `query()`，通过 `--resume` 加载原会话，不保证复用同一个 CLI 进程。`initialize`、审批、hook、MCP 以及 `interrupt()` 都是控制消息。初始化必须及时响应，不能因为共享槽位已满就阻塞整个 CLI 的启动，否则 SDK 可能在模型任务开始前超时。

## 哪些行为需要限制

| 行为 | 获取或占用槽位 | 检查提交间隔 |
| --- | --- | --- |
| 普通 `claude -p` / `--print`，含文本、JSON 输出及管道输入 | 在真实 CLI 启动前获取，保持至进程退出 | 启动前检查一次 |
| 普通交互会话：`claude`、`claude "prompt"` | 启动前获取，整个交互进程占用，包括空闲时间 | 启动前检查一次；终端内部后续轮次无法通过原始 stdio 单独识别 |
| `--resume` / `-r`、`--continue` / `-c`、`--fork-session` 的普通会话 | 与新会话相同，不因续聊或 fork 绕过 | 启动前检查 |
| SDK 双向 stream-json 进程启动、`initialize` | 只启动协议进程，不占槽位 | 不等待 |
| stream 第一条 `type: "user"`，包括新会话、续聊、图片输入、斜杠命令 | 转发前获取，保持至 CLI 退出 | 转发前检查 |
| 同一 stream 进程的后续用户消息或运行中追加输入 | 沿用该进程的一个槽位 | 每条转发前重新检查 |
| `generate_session_title`、`side_question` 及未识别的控制请求/消息 | 获取或复用进程槽位 | 每条转发前检查；未识别操作交给真实 CLI 校验 |
| 工具/权限/hook/MCP 的 `control_response` | 沿用已有任务，不另占槽位 | 不等待，防止阻塞任务继续执行 |
| `interrupt`、`stop_task`、`control_cancel_request` | 不新占槽位；interrupt 同时取消代理尚未转发的输入 | 不等待 |
| `set_permission_mode`、`set_model`、初始化、配置/上下文查询、MCP 配置和认证、文件读取/回滚、反馈 | 不新占槽位 | 不等待 |
| `--help`、`--version`、auth、mcp、plugin、config、doctor、update、install、setup-token 等管理命令 | 透传，不占槽位 | 不等待 |
| `agents --json`、logs、stop / kill、rm 等后台会话查询和管理 | 透传，不占槽位 | 不等待 |
| 脱离当前进程或在外部派发任务的入口 | 拒绝 | 不启动 |

已拒绝的派发入口包括 `remote-control`、交互式 `agents`、daemon、attach、respawn、self-hosted-runner、gateway、ultrareview，以及 `--bg` / `--background`、`--cloud` / `--remote`、`--environment`、`--desktop`、`--teleport`、`--sdk-url`、Remote Control 和 channel 相关选项。stream 控制请求 `remote_control`、`channel_enable`、`ultrareview_launch` 同样拒绝，避免先通过初始化再开启另一条派发路径。

并发单位是**CLI 会话任务进程**，不是内部 HTTP 请求或子代理。一次已入场任务内部的模型循环、工具调用、前台/后台 Agent、自动 compact、模型重试、后台任务通知和 Monitor 后续轮次共用该槽位；这些内部调用不逐次 sleep。需要限制会话内部的子代理并发或每次 HTTP 调用时，应在 Claude 配置或上游 API 层另行控制。普通终端模式的内部斜杠命令、配置/工具自行派生的独立服务不属于 stdin stream 的识别范围，不能把本代理视为对同 UID 任意程序的安全隔离。

## sleep、后台任务与释放时机

Claude 沿用现有的 **3–15 秒随机提交间隔**，不是每次固定 sleep：每条提交只抽取一次秒数，获得槽位后获取 `.claude-proxy/state.lock`，读取 `state.json` 的 `last_claude_task_at`。时间差不足时等待差值；首次任务或间隔已足够时立即提交。等待结束后重新读取当前时间，原子更新状态，再启动真实 CLI 或转发模型输入；锁保持至该提交已发出。状态损坏或锁/写入失败会拒绝提交。时钟回退时等待完整抽取间隔。

stream 等待在单独的 worker 中执行，协议循环继续转发审批回应、初始化、查询和中断。最多保留 128 条未提交输入，总正文上限 32 MiB，单条消息上限也是 32 MiB。输入按连接内接收顺序提交；多个代理之间不保证排队顺序。排队超时、无效配置、超量或转发失败会终止该 stream 连接，以 stderr 和退出码报告错误，不伪造 CLI 的 `result`。

`result` 只表示一轮结束，不能据此释放进程槽位。ClaudeCodeUI 可能保持 stdin 打开，让后台任务继续执行、触发自动后续轮次，最长等待窗口由其 runtime 设为 30 分钟。代理从第一条模型输入获准后一直持锁到真实 CLI 退出；纯初始化、从未提交任务的进程不占用额度。通用 SDK 如果长期保持已执行任务的进程打开，也会长期占用槽位，需由客户端关闭不再使用的 query。

stdin EOF 是输入半关闭：代理先完成尚未转发的排队输入，再关闭 CLI stdin，等待真实 CLI 完成并退出。它与 Codex app-server 的断连即停止语义不同。`interrupt` 取消代理尚未转发的所有输入，并立即传给 CLI；已经转发的任务仍由 CLI 处理中断，进程退出前不会提前释放槽位。

普通任务先启动一个持槽位 FD 的 Go launcher；launcher 在守护进程就绪、时间戳更新后才 exec 原生 Claude。stream 在第一条模型输入发出前创建相同的槽位守护进程。即使原生程序丢弃继承 FD，guard 仍保留锁；代理被 SIGKILL 时控制管道断开，guard 停止真实进程/进程组，确认消失后才释放。系统未及时回收孤儿进程时保守地保持占用。

SIGINT、SIGTERM、SIGHUP 会取消等待并停止真实进程，返回 `128 + signal`；非终端及 stream 使用独立进程组，清理时先 SIGTERM，500ms 后未退出则 SIGKILL。终端交互保留前台进程组，只对真实 CLI PID 清理。正常退出同样在回收 CLI 后释放锁，不自动重启或重放输入。

## macOS 与日志

macOS 模型会话要求 `CLAUDE_CONFIG_DIR=/xxx/{用户}/.claude`，从其父目录派生 `/xxx/{用户}`，复用 Codex 的外层 `sandbox-exec` 文件访问规则。管理命令不使用该外层沙箱。Claude 的工具权限、SDK `canUseTool`、`--permission-mode plan` 以及其他调用参数保持不变；不强制 `bypassPermissions`。

Claude 自身的 shell sandbox 设置也保留。如果已有配置启用了内部 sandbox，而系统拒绝嵌套 sandbox-exec，应在对应 Claude 配置中处理这一设置；代理不会悄悄移除外层目录隔离或降级启动。

日志是 `.claude-proxy/claude-proxy.log`，沿用文件锁和轮转。只记录固定模式、生命周期、进程/提交标识、guard PID、运行时用户目录名、工作目录、等待时长、上限和错误阶段，不记录 argv、stdin、输出、工具内容、认证环境或可能含提示词的原生错误文本。`user` 来自绝对 `CLAUDE_CONFIG_DIR` 的 `.claude` 父目录名，只用于日志；路径无效或缺失时为空。原生 CLI stdout / stderr 正常转发给调用方。等待槽位超时返回 `124`，配置、协议、锁、沙箱或启动错误返回 `2`，正常执行返回真实程序退出码。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

Go 测试覆盖普通任务与 SDK 共享 Claude 槽位、Codex/Claude 额度分离、控制回应不被等待阻塞、半关闭排队处理、result 后持锁、中断取消、重复输入间隔、信号回收、损坏状态拒绝和原生程序关闭继承 FD 后 guard 仍持锁。

安装 Claude Agent SDK 后还可验证真实 SDK 的 transport，仍只使用本地模拟 CLI，无 API 调用或认证需求：

```sh
CLAUDE_PROXY_TEST_SDK_MODULE=/absolute/path/to/@anthropic-ai/claude-agent-sdk/sdk.mjs \
  go test ./internal/proxy -run '^TestClaudeAgentSDKCompatibility$' -v
```

该测试需要 `node`，覆盖 SDK 初始化、resume、plan 模式、双轮输入、工具权限回调、切换权限模式/模型和 interrupt。生产代码没有 Node 或 SDK 依赖。
