# codex-proxy

使用 Go 开发的 Codex CLI 拦截器，要求 **Go 1.23.4 或更高版本**，支持 macOS、Linux。生产代码不依赖第三方 Go 包。

- `codex exec`、`codex e` 及其子命令按同机、同用户共享并发额度，满额等待。
- `codex app-server` 默认拒绝，退出码 `1`；启用配置后由 fake codex 直接启动并经 stdio 代理真实 app-server，执行模型任务时与 `exec` 共享并发额度。
- 成功占用槽位后，每次任务提交随机抽取 3–15 秒的间隔；`exec` 和 app-server 共用上次提交时间，间隔不足时等待差值。
- 其他命令透传给真实 Codex，不占用执行槽位。
- `dsh`、`pi` 提供独立的轻量沙盒入口，macOS 未处于沙盒时通过 `exec` 交给 `sandbox-exec`，已处于沙盒时直接执行真实程序，原样传递所有参数。

## 构建及接入

```sh
./build.sh
```

脚本可从任意工作目录调用，自动创建项目下的 `bin/` 目录，输出 `bin/codex`、`bin/dsh` 和 `bin/pi`。构建成功后，在当前 UID 的系统账户主目录下创建 `.codex-proxy/`（权限 `0700`），并将 [env.example](env.example) 和 [sandbox.sb](sandbox.sb) 分别复制为 `.codex-proxy/env.example`、`.codex-proxy/sandbox.sb`（新建文件权限 `0600`）；已有文件会保留。

macOS 和 Linux 必须设置 `CGO_ENABLED=1`（脚本会自动设置）：macOS 通过系统 `sandbox_check` 接口检测当前进程是否已处于沙盒，Linux 通过 libc 查询系统账户（支持 NSS）。两者都不要使用 `osusergo` 构建标签。关闭 cgo 时，需要这些能力的入口会明确失败，不会跳过检测或使用 `HOME` 回退。构建需要 C 编译器；macOS 需要 Command Line Tools，Linux 需要目标系统的 libc。

将编译产物所在目录放在 `PATH` 最前面。例如将固定的绝对路径写入正常的 shell 初始化配置：

```sh
export PATH="/absolute/path/to/codex-proxy/bin:$PATH"
```

保留真实 Codex、dsh 和 pi 安装，不要用包装程序覆盖原始可执行文件。各入口都必须明确配置真实程序的路径，不通过 `PATH` 查找。

## 配置

代理按当前 UID 查询系统账户数据库，从账户记录取得原始用户目录，固定读取：

```text
<系统账户目录>/.codex-proxy/env
```

运行时修改 `HOME`、`USER`、`CODEX_HOME`、`DSH_HOME`、`PI_CODING_AGENT_DIR` 不会改变代理配置、锁或日志位置；真实程序继续使用调用方传入的环境与工作目录。账户查询失败时返回配置错误。

运行 `./build.sh` 后，将系统账户目录下的 `.codex-proxy/env.example` 复制为同目录的 `env`，并修改真实路径。脚本不会创建或覆盖实际配置 `env`。也可手动创建 `.codex-proxy`，将项目中的 [env.example](env.example) 复制为其中的 `env`。请使用账户记录中的绝对目录，而非可能已修改的 `$HOME`。

```dotenv
CODEX_PROXY_REAL_BIN=/absolute/path/to/native/codex
DSH_PROXY_REAL_BIN=
PI_PROXY_REAL_BIN=
CODEX_PROXY_APP_SERVER_ENABLED=false
CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT=10m
CODEX_PROXY_MAX_CONCURRENCY=1
CODEX_PROXY_WAIT_TIMEOUT=10m
CODEX_PROXY_STATE_LOCK_PATH=
CODEX_PROXY_STATE_PATH=
CODEX_PROXY_LOG_MAX_SIZE_MIB=10
CODEX_PROXY_LOG_MAX_BACKUPS=5
```

| 配置键 | 默认值 | 含义 |
| --- | --- | --- |
| `CODEX_PROXY_REAL_BIN` | 必填 | 真实原生 Codex 的绝对路径 |
| `DSH_PROXY_REAL_BIN` | dsh 必填 | 真实 dsh 的绝对可执行路径；Codex 忽略此配置 |
| `PI_PROXY_REAL_BIN` | pi 必填 | 真实 pi 的绝对可执行路径；Codex 和 dsh 忽略此配置 |
| `CODEX_PROXY_APP_SERVER_ENABLED` | `false` | 设为 `true` 启用 app-server 代理；未配置或 `false` 保持拒绝 |
| `CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT` | `10m` | app-server 无请求且无待处理工作时的退出时限；`0` 禁用；新连接读取 |
| `CODEX_PROXY_MAX_CONCURRENCY` | `1` | 正整数，同时执行的上限 |
| `CODEX_PROXY_WAIT_TIMEOUT` | `10m` | 排队超时，支持 `30s`、`10m`；`0` 无限等待 |
| `CODEX_PROXY_STATE_LOCK_PATH` | 空 | `state.lock` 的绝对文件路径；未配置或为空使用 `.codex-proxy/state.lock`；进程启动时读取 |
| `CODEX_PROXY_STATE_PATH` | 空 | `state.json` 的绝对文件路径；未配置或为空使用 `.codex-proxy/state.json`；进程启动时读取 |
| `CODEX_PROXY_AUTH_DIR` | 空 | 多账号凭据目录的绝对路径；未配置或为空关闭账号轮换；进程启动时读取 |
| `CODEX_PROXY_LOG_MAX_SIZE_MIB` | `10` | 每个日志文件的上限，1 MiB = 1,048,576 字节 |
| `CODEX_PROXY_LOG_MAX_BACKUPS` | `5` | 非负整数，备份数量，不含当前日志；`0` 不保留备份 |

支持空行、`#` 注释、可选 `export` 前缀、单引号或双引号，以及空白后的行尾注释。双引号支持 Go 字符串转义。不会执行 shell、展开 `$VAR`、`~` 或命令替换。未知键会被忽略；重复键、语法错误以及已知配置项的无效值会报错。进程中同名环境变量不会覆盖文件配置。

### Codex 多账号轮换

在系统账户的 `.codex-proxy/env` 中配置：

```dotenv
CODEX_PROXY_AUTH_DIR=/xxx/codex-auth
```

该目录第一层的凭据文件必须是普通文件，且名称匹配 `^auth[0-9]+_.+\.json$`，例如 `auth0_alice.json`、`auth1_张三.json`。下划线后的账号名称不能为空；目录、符号链接和其他名称不参与选择。按文件名字典序排序，因此 `auth10_carol.json` 位于 `auth1_bob.json` 之前、`auth2_dave.json` 之前。启动和换号时重新扫描，提交任务前重新检查当前账号文件及其共享冷却状态。

首次初始化软链时从第一个可用文件开始；同一 `CODEX_HOME` 的后续进程复用软链指向的可用账号，允许同账号并发和嵌套 `codex exec`。原生模型任务报告 429、`usageLimitExceeded` 或 `rateLimitExceeded` 时，立即异步记录冷却状态并停止真实 Codex；确认子进程退出后释放认证读锁，非阻塞尝试一次认证写锁。拿到写锁后重新读取软链和共享状态：其他进程已选好可用账号时保留该账号，否则从当前账号的下一个文件开始循环查找并原子替换软链。拿不到写锁就跳过本次换号，不等待其他读者退出；冷却状态仍然必须保存。最后代理以退出码 **75**（临时失败）退出，由调用方重新启动和重试。代理不重启真实进程、不恢复线程、不重放失败任务。原生错误或限流信息提供恢复时间时使用该时间，无法获取时默认冷却 60 秒。只有已经耗尽的限流窗口参与恢复时间计算，单纯收到用量通知不触发账号禁用。`exec` 保持原始 stdout/stderr，仅识别原生错误事件和明确的错误诊断，不把成功的工具输出中的“429”当作账号限流；需要完整结构化错误时建议使用 `exec --json`。

账号状态直接放在现有 `state.json` 的 `codex_accounts` 中，没有目录层：

```json
{
  "last_codex_task_at": 1792746000,
  "codex_accounts": {
    "auth0_alice.json": {
      "cooldown_until": 1792749600,
      "last_429_at": 1792746000
    }
  }
}
```

时间均为秒级 Unix 时间戳。文件没有状态记录或当前时间达到 `cooldown_until` 时可用，冷却到期不清零、不删除历史记录。429 更新复用 `state.lock`，持锁重新读取最新内容后，两项时间分别取 `max(已有值, 本次值)`，保留其他账号、任务时间和未知字段，再原子替换写入。跨进程仅共享账号冷却状态，不强制避免重复的 429。同一个状态文件对应同一套凭据池，完整文件名是状态标识；替换同名文件后，其原冷却状态继续生效。

目录中没有匹配文件或所有账号均在冷却中时，`exec` 在申请并发槽位和等待提交间隔之前立即拒绝，不等待冷却恢复、不延长账号冷却时间。取得槽位后才获取认证读锁并重新检查账号；等待槽位的 `exec` 不会固定凭据或阻碍换号。当前账号已冷却且其他原生进程仍持读锁、无法安全换号时，也返回 429。app-server 启动失败立即输出 `id: null`、`error.code: 429` 的 JSON-RPC 错误并退出，不等待客户端发送初始化请求。已运行的 app-server 在提交新任务前发现当前账号已冷却时，也返回 429 并关闭连接；提交前在 `state.lock` 下再次检查，避免排队期间错用已冷却账号。`exec` 和 app-server 都在 stderr 明确报告 429，进程退出码统一为 **75**；系统退出码不能直接使用 429。原生模型错误的响应或通知原样透传，随后关闭连接。目录不可访问、状态损坏或状态持久化失败属于配置/存储错误，不能视为未启用或无账号。

启用后要求调用方提供绝对路径的 `CODEX_HOME`，并强制真实 Codex 使用 `cli_auth_credentials_store="file"`。选中的账号通过 `CODEX_HOME/auth.json` 的绝对软链使用，采用临时软链加原子重命名替换；原有普通 `auth.json` 首次替换前保留为 `auth.json.codex-proxy-backup`，不覆盖已有备份。凭据和 token 不写入代理状态或账号事件日志。此模式针对 Codex 的文件认证；自定义 provider 的环境变量认证不由该账号池管理。

原生 app-server 会缓存认证，因此一个模型任务触发账号 429 后，会关闭整个真实 app-server 及其进程组，并结束代理的 stdio 连接，同进程其他会话也会终止。调用方负责重新拉起代理；需要继续会话时，由调用方恢复持久化线程。临时线程无法恢复。MCP、OAuth 等外部服务的 429 不触发 GPT 账号冷却或进程终止，普通用量通知也不会触发退出。开启自动凭据管理后，客户端的 `account/login/start` 和 `account/logout` 被拒绝，避免改写或删除凭据池中的账号文件。

429 关闭期间会继续收集原生进程已经输出的模型限流错误和恢复时间提示，合并最大的冷却截止时间并保存，再释放认证读锁和尝试换号；此阶段不会提交新任务。单纯的用量通知仍不触发账号冷却，外部服务的错误也不会延长 GPT 账号的冷却时间。

同一个 `CODEX_HOME` 的多个代理连接通过 `.codex-proxy-auth.lock` 共享读锁，固定真实 Codex **整个进程生命周期**中的软链目标；app-server 空闲时仍持有读锁。正常退出在真实进程停止后释放读锁；429 退出同样先停止真实进程，再释放读锁、独立尝试写锁，禁止直接升级仍在使用的读锁。原生子进程继承读锁 FD，代理被强杀时仍保护认证软链；任务守护进程继续承担已有的任务槽位清理。不同 `CODEX_HOME` 独立协调，仍受原有共享并发上限约束。macOS 沙箱只为本次选中的凭据文件增加读写例外，不放开整个账号目录或其他用户目录。由于 `.codex-proxy` 目录禁止访问，macOS 沙箱内再次启动 fake codex 会因无法访问配置或锁目录而失败。

启用账号池的 `exec` 在真实进程退出后，最多继续等待输出管道排空 500 毫秒。后台进程继续持有 stdout/stderr 时，到期关闭代理的输出读取管道，随后释放并发槽位和认证读锁；保留真实 Codex 的退出码，识别到 429 时仍以 75 退出。

纯文本错误识别兼容 `exec --color always`；ANSI 颜色控制码仅从识别副本中移除，原始 stdout/stderr 保持不变。

`CODEX_PROXY_AUTH_DIR` 未配置或为空时完全不参与账号扫描、凭据存储覆盖或软链切换。此项与状态路径一样在代理启动时读取，修改目录配置需要重启现有代理连接。

### 定位真实二进制

`REAL_BIN` 必须指向原生 ELF/Mach-O 可执行文件，不能指向代理自身、shell 脚本或 npm 的 `codex.js` 启动器。npm 启动器会另外创建进程且不传递槽位 FD，无法满足强杀代理后继续持锁的要求。

如果通过 npm 安装，先查看原始 `codex` 链接和 `@openai/codex` 包内的 `bin/codex.js`，从平台包目录找到 `vendor/<target>/bin/codex`。布局随安装工具变化；常见示例：

```text
.../@openai/codex/node_modules/@openai/codex-darwin-x64/vendor/x86_64-apple-darwin/bin/codex
.../@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex
```

升级后若实际路径改变，需要更新配置。首次接入及 Codex 升级后，应在测试环境核对原生进程能保留继承的槽位锁描述符。

### 运行时更新

每次并发判断重读文件，默认每 100ms 重试。推荐先写完整临时文件，再通过同目录 `mv` 原子替换 `env`，避免读取半写入配置。无效配置会停止放行，已有任务继续运行。

提高上限后，等待任务可以进入；降低上限时，已有任务继续运行，直到占用数量小于新上限才放行。超时从最初开始等待计算，修改配置不会重置计时；超时只限制获取槽位的等待，不限制占用槽位后的提交间隔等待或运行时间。不保证排队顺序。

状态路径配置只影响新启动的代理进程，已有 app-server 连接继续使用启动时的路径。修改这两项前，应先停止该账户的所有代理及其任务，完成迁移后统一重启，避免新旧路径形成两套独立的状态锁。

## app-server 代理

在系统账户目录的 `.codex-proxy/env` 中显式配置以下开关，才会启用代理：

```dotenv
CODEX_PROXY_APP_SERVER_ENABLED=true
```

默认关闭。配置文件缺失、开关未配置或设为 `false` 时，fake codex 拒绝所有被拦截的 app-server 命令（包括 `--help` 和协议生成子命令），保持原来的提示和退出码 `1`，不启动真实程序、不占槽位。开关只读取该文件，进程环境变量无法覆盖；无效布尔值返回配置错误 `2`。启用判断和子进程管理均由 fake codex 负责。修改开关影响 fake codex 接受的新连接，已有连接继续运行，并发和等待参数仍在每次任务入场时重新读取。

每次调用 `fake codex app-server` 都使用以下进程链路，无需后台服务：

```text
客户端 ⇄ fake codex app-server ⇄ 真实 codex app-server
       stdio                  stdio
```

fake codex 在自身进程内处理逐行 JSON-RPC，并用调用方的参数、工作目录和环境直接启动真实 app-server 子进程。任务获取共享并发槽位、完成基于 state 的提交间隔等待后才转发给子进程；查询和审批回应不被等待阻塞。请求 ID 会在内部连接上重新分配，在返回客户端时恢复，避免与代理自己的设置请求冲突。真实程序的 stderr 直接转发到调用方 stderr。代理只支持默认 stdio 或 `--listen stdio://`，拒绝 WebSocket、daemon、proxy 子命令和远程控制入口，避免任务绕过共享锁。`--help`、`generate-ts`、`generate-json-schema` 直接透传，不占槽位。

使用方式与原命令相同，客户端仍连接 fake codex 的 stdin/stdout：

```sh
CODEX_HOME=/xxx/alice/.codex codex app-server
```

### 进程归属与空闲退出

每次 `fake codex app-server` 调用对应一个真实 app-server 进程，连接内可以有多个线程；没有按项目目录去重或复用。同一项目的两个客户端调用会创建两个真实进程，但任务仍共享同一账户的并发额度。[官方协议](https://learn.chatgpt.com/docs/app-server)允许 `thread/start` 和 `turn/start` 指定 `cwd`，因此协议本身并不要求一个项目只能有一个进程。

默认连续空闲 10 分钟后，fake codex 终止并回收真实 app-server 的进程组，再以退出码 `0` 退出。客户端保持 stdin 打开不会阻止回收。新客户端消息会重置空闲计时；存在以下任一工作时暂停计时：等待并发槽位、等待 state 提交间隔、未完成的 RPC 或审批/工具请求、运行中的 turn/shell、尚未收到 `process/exited` 的 `process/spawn` 进程、活动 goal、realtime 会话，以及尚未清空的队列（包括暂停的队列）。全部工作结束后重新获得完整的空闲窗口，已加载但未运行的线程不阻止退出。

`CODEX_PROXY_APP_SERVER_IDLE_TIMEOUT=10m` 可设为其他非负时长，`0` 禁用空闲回收。此项在连接启动时读取，修改后对新连接生效。退出会关闭客户端连接，之后由客户端重新启动 `fake codex app-server`，执行 `initialize` / `initialized`，并通过 `thread/resume` 恢复已持久化线程；代理不自动重启或重放请求，临时线程不保证可恢复。日志以 `shutdown` 事件和 `reason: "idle_timeout"` 记录正常回收。

### 协议和任务控制

已按 Codex 0.162.1 的协议验证。代理需要真实程序支持 `thread/settings/update`；在内部 `initialize` 请求中启用 `experimentalApi`，以便普通客户端也能应用该设置。线程创建、恢复、fork 后，以及任务首次放行前，强制设置 `approvalPolicy: "never"` 和以下策略：

```json
{"sandboxPolicy":{"type":"externalSandbox","networkAccess":"enabled"}}
```

线程创建接口的 `sandbox` 参数只能使用枚举，因此先以 `danger-full-access` 创建线程，再通过内部设置请求应用 externalSandbox。任务及设置请求中的相冲突策略会被覆写；无法应用策略时拒绝任务。macOS 外层继续使用下文的 `sandbox-exec` 文件隔离规则。

fork 的临时线程和预测模式（`experimentalPredictionMode: true`）有额外兼容处理。预测 fork 必须由已加载的持久父线程创建，且原生禁止它与其他配置覆写组合，因此代理不向该请求注入 `sandbox`、`approvalPolicy` 或 `config`；普通临时 fork 仍注入线程创建策略，但代理不会自动添加 `deferGoalContinuation`，因为原生禁止它与 `ephemeral: true` 同时出现。持久化 fork 继续强制 `deferGoalContinuation: true`，防止复制的 goal 在任务入场前自动续跑。客户端自行传入的非法组合（例如预测 fork 带配置覆写、临时 fork 带 `deferGoalContinuation: true`）保持原样交给原生校验，代理不会静默改写成另一种操作。所有 fork 成功后、向客户端返回结果前，仍通过内部 `thread/settings/update` 强制 `externalSandbox` 和 `approvalPolicy: "never"`。预测或临时子线程本身只得到空闲线程，不占用槽位；其后续 `turn/start` 等模型任务与父线程无关地独立申请并持有槽位。

| 协议操作 | 并发控制 |
| --- | --- |
| `turn/start` | 新任务获取槽位；运行中的同一 turn 补充输入沿用原槽位 |
| `review/start`、`thread/compact/start` | 持锁直到相应 turn 完成、失败或中断；detached review 独立占用槽位 |
| `thread/approveGuardianDeniedAction` | 恢复任务前获取槽位，已运行任务沿用原槽位 |
| `thread/prediction/request` | 按 `threadId`+`sourceTurnId` 获取槽位，对应 `thread/prediction/updated` 结果通知或 RPC error 后释放，不消耗同一线程上 compact/review/turn 的等待；真实程序不支持时透传其错误 |
| `thread/realtime/start` | 获取槽位，会话关闭且已观察到的 turn 结束后释放；追加输入不重复获取 |
| `thread/goal/set` 激活、新目标或恢复 goal | 整个自动续跑链持有一个槽位，暂停、结束或中断且当前 turn 结束后释放 |
| `thread/goal/set` 暂停、blocked 等纯状态/预算补丁 | 不启动执行；已有链持有槽位时直接控制，不额外占槽；显式非 active 状态会取消等待中的激活 |
| `thread/queue/add`、list、update、delete、reorder | 操作队列不占槽位，每条消息实际开始执行前重新获取槽位 |
| `thread/unsubscribe`、`turn/steer`、审批或工具回应、查询、登录、线程元数据、shell/command/process/fs 操作 | 不额外占槽位，也不等待任务提交间隔 |

只有需要执行模型任务的白名单操作进入并发和提交间隔控制。其他 RPC 默认转发给真实 app-server；即使其他任务正在等待槽位或 sleep，查询和控制请求仍能转发。非白名单并不表示完全不处理协议：初始化通知偏好、订阅过滤、代理队列和强制执行策略仍由代理管理；账号池启用时的登录/退出及远程任务入口仍受前文限制。

goal 的部分更新按线程 ID 缓存目标与状态，不把子线程的 goal 元数据放到共享执行链上。省略或为 `null` 的 `status`/`objective` 沿用它已有的状态；显式 `status: "active"`、与当前不同的 objective 会申请执行槽位；未知状态或已知 active 但尚未持槽的补丁也经过入场控制。已知 paused/blocked/usageLimited/budgetLimited/complete 的预算或同目标补丁，属于控制操作，不占槽且不会取消正在等待的激活；只有显式停用状态或 `thread/goal/clear` 才取消等待。`origin` 原样透传，不由代理补写。线程 start/resume/fork 成功后，代理尽力查询并缓存 goal；临时线程不支持 goal 时查询失败不影响线程创建。激活在途时由该 RPC 保持预留，原生确认成功后才标记自动续跑；激活失败即使与预算补丁交错也不会留下虚假的 active 标志。迟到的 goal/get/set 回包若已被确认生效的暂停、清除或通知超过，不会覆盖新状态；尚未确认的暂停不会提前释放仍在运行的链。

队列由代理在内存中管理，通过 `turn/start` 执行每条消息，避免真实 app-server 的自动出队绕过 admission。每个线程最多保存 1,024 条待执行消息，连接内队列输入总量最多 32 MiB；单条协议消息最多 32 MiB，同时等待槽位的任务最多 128 个。入队后空闲线程自动执行；超时、失败、中断后暂停队列，可通过 `thread/queue/start` 恢复。自动出队被原生以 JSON-RPC error 拒绝时，代理暂停剩余队列、保留失败和后续消息，并发出带 `queuedSubmissionId`/`clientUserMessageId` 的 `error` 通知供客户端关联；通知的 `error.message` 附带原生错误的 code/message/data，代理日志也保留同一原因以便内部排障。显式 `thread/queue/start` 的失败仍以原客户端请求 id 返回，成功后该条消息才从队列移除。条目已发送但尚未收到原生 ACK 时，修改、删除该条目或重排队列会返回 `-32000`，避免确认成功的编辑被旧 ACK 丢弃；其他未发送的条目仍可编辑，原生拒绝后失败条目也可修改再重试。客户端断开时队列随 fake codex 退出清空，不持久化。持久化 fork 强制 `deferGoalContinuation: true`，复制的 goal 由下次显式 turn 或 goal 恢复请求启动；临时 fork 不添加该字段，预测 fork 也不改写其他配置。代理在新 turn 放行前查询 goal，确保复制或恢复的自动续跑链继续持锁。

每次新任务申请及每次等待重试都重新读取同一个 `.codex-proxy/env`，使用与 exec 完全相同的文件锁。等待不会阻塞协议查询或审批回应。等待超时返回 JSON-RPC 错误 `-32001`，取消返回 `-32800`，配置、锁或策略设置失败返回 `-32000`；代理继续处理其他请求。尚未提交原生的任务通过 `$/cancelRequest` 携带原客户端请求 ID 取消。

`turn/interrupt` 始终转发给原生，由原生校验活动 turnId 并返回结果；成功后才暂停该线程的本地队列，并取消发送 interrupt 时该线程正在等待提交的输入。没有活动 turn、缺少 turnId 或 turnId 不匹配时，原生错误照常返回，代理不提前取消等待任务或暂停队列。`thread/goal/set` 的停用状态、`thread/goal/clear`、`thread/realtime/stop` 同样在原生成功确认后才取消对应的等待激活；原生拒绝时保留等待任务。线程 shell 的启动记账按 threadId 区分，父模型任务结束不会把子线程的独立 shell 误判为未入场的模型任务。

初始化和策略覆写只处理符合预期结构的参数；类型错误的 `params`、`config` 或初始化 capabilities 保留给原生校验，避免代理把无效请求改写成成功的线程创建或初始化。普通转发保留未知方法、未知字段和原生错误内容，仅映射并恢复客户端请求 ID。

初始化的 `optOutNotificationMethods` 由代理在处理内部生命周期后过滤，内部原生连接仍接收任务完成通知，避免客户端屏蔽通知后泄漏槽位。`thread/unsubscribe` 对空闲线程立即转发并返回原生结果；运行中则先返回 `unsubscribed` 并停止客户端 turn/item 通知，保留内部订阅直到该线程的任务完成、失败或取消，再发送真实取消订阅请求。等待期间重复取消返回 `notSubscribed`，原生卸载后返回 `notLoaded`；显式 `thread/resume` 恢复订阅，也会撤销尚未发出的延迟卸载。线程状态和元数据广播仍可见，`thread/closed` 只转发真实原生通知，不由代理生成。订阅和卸载按实际线程 ID 分别处理，不影响共享任务槽位的父子线程。RPC 回包、服务器向客户端发出的请求以及错误通知不受通知偏好过滤。

代理为原生 app-server 强制添加 `-c thread_unload_delay_secs=0`，覆盖默认的无订阅空闲宽限期；运行中任务的排空由代理完成，真正取消订阅后可及时收到关闭通知。在 Codex 0.163.0 和 0.165.0 上均已验证 `thread/unsubscribe` → `thread/closed` → `thread/resume`（原 threadId、新 modelProvider）可切换服务并保留历史，同一 app-server 中其他线程继续使用原 provider。仅 resume 尚未卸载的线程不会完成这种切换。

线程收到 `thread/closed`、`thread/deleted`、`thread/archived` 或真正的 `notLoaded` 状态时，代理取消该线程尚在等待槽位、提交间隔或策略/goal 准备的 admission，返回 `-32800` 并释放它已取得的预留，清空该线程失效的 `policyReady`、goal 缓存和订阅状态；随后才到的 admitted/state_ready、私有回包和任务 ACK 不再恢复旧任务状态；创建请求在私有初始化期间被关闭会收到取消错误。按 `path` 恢复时，关闭检查使用原生返回的实际目标线程及其最后关闭时间，不依赖可能被原生忽略的请求 `threadId`。`thread/unsubscribe` 的延迟阶段不取消仍在运行的任务，也不推进任务生命周期。子 agent 与 parent 共享一个执行链时按线程 ID 分别记录 inflight/awaiting/goal/realtime 标志，关闭 child 不会释放或清空 parent 仍在使用的槽位和状态；detached review 是独立执行链，源线程关闭不会误释放它的额度。准备卸载时暂停该线程队列，已排队但未执行的消息保留，重新加载线程后可通过 `thread/queue/start` 恢复。

客户端 EOF、SIGINT、SIGTERM、SIGHUP 时，fake codex 取消等待，终止真实 app-server 的进程组；500ms 后仍未退出则 SIGKILL，并在回收进程后释放锁。每个持锁任务另有一个继承槽位 FD 的守护进程：fake codex 被 SIGKILL 时，控制管道断开，守护进程终止真实程序，确认进程组消失后才释放锁。若系统迟迟不回收孤儿进程，槽位会保守地保持占用。

## macOS 执行沙箱

macOS 的 `exec`、`e` 及其子命令和 app-server 模式通过 `sandbox_check(getpid(), NULL, 0)` 检测当前进程：未处于沙盒时使用 `/usr/bin/sandbox-exec` 启动真实原生 Codex；已处于沙盒时直接启动真实 Codex，继承外层沙盒，不再生成 profile 或读取 `sandbox.sb`。检测失败直接报错，不回退。此判断不依赖进程环境变量，也不改变 Codex 的并发控制、任务提交前的 sleep、认证处理或禁用内部沙盒的参数。

需要创建沙盒时，调用方必须设置绝对路径的运行时环境变量 `CODEX_HOME=/xxx/{用户}/.codex`，代理据此派生共享父目录 `/xxx` 和当前用户目录 `/xxx/{用户}`。这不会改变代理配置、锁或日志的系统账户目录。已有沙盒的权限范围沿用外层规则，不会因内层 HOME 配置而改变。

沙盒检测不放宽文件权限：配置、锁、状态和日志仍按原路径访问。若外层规则禁止访问 `.codex-proxy`，嵌套代理仍会因这些文件不可访问而失败。

```sh
CODEX_HOME=/xxx/alice/.codex codex exec --json "任务描述"
```

代理按以下规则生成 profile，并直接作为 `-p` 参数传递，不经过 shell：

```scheme
(version 1)
(allow default)
(deny file-read* file-write*
    (require-all
        (subpath "/xxx")
        (require-not
            (subpath "/xxx/alice"))
    )
)
(allow file-read-metadata
    (literal "/xxx"))
(allow file-read-metadata
    (subpath "/xxx"))
; 此处附加系统账户 .codex-proxy/sandbox.sb 的内容。
(deny file-read* file-read-data file-read-metadata file-read-xattr
      file-write* file-write-create file-write-data file-write-flags
      file-write-mode file-write-owner file-write-setugid file-write-times
      file-write-unlink file-write-xattr
    (subpath "/system-account/.codex-proxy"))
```

当前用户目录允许读写，其他用户目录的读写及元数据访问被拒绝；共享父目录仅允许读取元数据，不允许列目录。固定工作区目录及其子路径额外允许元数据查询；此规则本身只放开元数据权限。系统账户目录下的 `.codex-proxy` 及其所有子路径始终禁止读写、列目录和元数据访问，包括配置、状态、锁、日志和附加规则文件；即使它位于当前用户目录或共享目录之外，也会限制。其余共享目录之外的路径按原方式访问。路径会解析已有祖先的符号链接（例如 `/tmp` 对应 `/private/tmp`），用户目录解析后必须仍是共享父目录的直接子目录；不接受共享父目录为 `/`。

可编辑系统账户目录下的 `.codex-proxy/sandbox.sb`，以 SBPL 规则片段追加限制，例如：

```scheme
(deny file-read* file-write*
    (subpath "/absolute/path/to/private"))
```

代理在启动沙箱子进程前读取文件，将内容附加到内置规则之后，再添加 `.codex-proxy` 的强制拒绝规则。该目录的拒绝规则同时覆盖具体读写操作，避免前面的元数据或具体操作放行规则将其重新开放；多账号模式增加所选认证文件例外时仍保留这些限制。因此 macOS 的凭据池目录应放在 `.codex-proxy` 之外。文件路径固定取自系统账户目录，不受运行时 `HOME`、`USER`、`CODEX_HOME` 影响。

`sandbox.sb` 不需要 `(version 1)` 或默认策略，只需规则片段；路径使用绝对路径，不展开 shell 变量。文件不存在或为空时只使用内置规则和目录保护规则；若手动部署，可自行创建此文件。文件必须是普通文件，禁止符号链接，内容为无 NUL 的 UTF-8 文本，最多 64 KiB；读取失败或格式不符合要求时拒绝启动。SBPL 语法由 `sandbox-exec` 校验，失败后不会回退到无沙箱执行。每次创建新沙盒时读取一次，已有进程不会随文件修改而改变规则；继承已有沙盒时不再读取。

真实 Codex 自动应用 `--skip-git-repo-check --dangerously-bypass-approvals-and-sandbox`：已有对应选项时不重复添加；已有 `-s` / `--sandbox` 时移除该选项及其值，包括附着值和 `=` 写法。若被移除的选项紧接 `--image` / `-i` 的独立图片值，图片参数会改为逐个附着值（例如 `--image=a.png --image=b.png`），保留图片列表与后续提示词的边界。其他参数（包括 `--experimental-json`、未知选项）、提示词及 `--` 后内容原样保留，参数校验交给真实 Codex。`--dangerously-bypass-approvals-and-sandbox` 跳过 Codex 自身的审批和沙箱，外层 `sandbox-exec` 继续应用上述文件访问规则。

配置缺失或路径无效时返回 `2`，且不会占用执行槽位。`sandbox-exec` 启动或应用规则失败时返回失败状态，不回退到无沙箱执行。launcher 保留 PID 和继承的槽位 FD，信号转发及并发控制继续有效。Linux 和其他 Codex 命令按原方式执行。

## dsh 沙盒入口

`bin/dsh` 接收的所有参数都会原样交给真实 dsh，包括未知选项、空参数和 `--` 后的内容；不添加 Codex 专用参数。必须在当前 UID 系统账户目录下的 `.codex-proxy/env` 中指定真实可执行文件的绝对路径：

```dotenv
DSH_PROXY_REAL_BIN=/absolute/path/to/real/dsh
```

`DSH_PROXY_REAL_BIN` 只从该文件读取，进程中的同名环境变量不会覆盖或补全文件配置。dsh 只使用文件中的这一项，不要求配置真实 Codex、启用 app-server 或通过其他 Codex 配置值的校验；文件本身的语法仍须合法。`DSH_HOME` 继续从调用进程的环境变量读取：

```sh
export DSH_HOME=/xxx/alice/.dsh
dsh --help
```

`env` 文件不存在、读取失败或语法不合法，以及 `DSH_PROXY_REAL_BIN` 缺失、为空、不是绝对路径或不是普通可执行文件时返回 `2`，不会从进程环境变量或 `PATH` 查找、回退。禁止指向包装程序自身及其软链、硬链，避免递归调用。配置路径固定取自系统账户记录，不受运行时 `HOME`、`USER` 或 `DSH_HOME` 影响。

macOS 上，未处于沙盒时，`DSH_HOME` 必须是 `/共享目录/用户目录/配置目录` 形式的绝对路径，末级目录名不限（示例为 `.dsh`）。使用与 Codex 相同的路径校验及文件访问策略，以 `DSH_HOME` 的父目录作为当前用户目录、再上一级作为共享目录。创建沙盒时读取当前 UID 系统账户目录下的 `.codex-proxy/sandbox.sb`，追加规则并强制禁止访问整个 `.codex-proxy`；文件要求和错误处理与 Codex 相同。运行时修改 `HOME`、`USER`、`CODEX_HOME` 不会改变规则文件的位置或用户隔离范围。

启动使用 `exec` 替换当前进程为 `/usr/bin/sandbox-exec -p <profile> <真实dsh> <原始参数...>`，由其继续执行真实 dsh。检测到已处于沙盒时，直接 `exec` 真实 dsh，继承外层限制，不校验用于生成 profile 的 `DSH_HOME` 或读取 `sandbox.sb`；真实路径仍从系统账户的 `env` 文件读取。PID、标准输入输出、工作目录和环境由调用链继承，不占用 Codex 并发槽位，不创建状态或日志，不代理请求。沙盒检测或启动失败时直接失败，不回退。Linux 同样从系统账户的 `env` 文件读取真实路径，再直接 `exec` 真实 dsh，不要求 `DSH_HOME`。

## pi 沙盒入口

`bin/pi` 沿用 dsh 的启动方式，原样传递所有参数，通过 `exec` 替换当前进程，继承 PID、标准输入输出、工作目录和环境。真实 pi 的绝对可执行路径配置在当前 UID 系统账户目录下的 `.codex-proxy/env`：

```dotenv
PI_PROXY_REAL_BIN=/absolute/path/to/real/pi
```

此项只从文件读取，进程中的同名变量和 `PATH` 不会覆盖或补全配置。pi 只校验自己的程序路径，不要求配置 Codex 或 dsh；env 文件仍需符合上述语法。真实程序可以是普通可执行文件或指向它的软链，包括带 shebang 的启动脚本。配置缺失、路径无效或指向包装程序自身及其软链、硬链时返回 `2`。

运行时通过 `PI_CODING_AGENT_DIR` 指定配置目录，例如：

```sh
export PI_CODING_AGENT_DIR=/Users/agent/workspaces/alice/.pi/agent
pi --help
```

macOS 未处于沙盒时要求此变量为 `/共享目录/用户目录/.pi/agent` 形式的绝对路径，可带尾斜杠。从 `agent` 向上两级取得用户目录，再向上一级取得共享目录；上例分别为 `/Users/agent/workspaces/alice` 和 `/Users/agent/workspaces`。沙箱允许访问整个当前用户目录，并限制其他用户目录，规则和路径校验与 dsh 相同。创建沙盒时追加系统账户 `.codex-proxy/sandbox.sb` 并强制禁止访问整个 `.codex-proxy`，与运行时 `HOME`、`CODEX_HOME`、`DSH_HOME` 无关。

macOS 未处于沙盒时使用 `/usr/bin/sandbox-exec -p <profile> <真实pi> <原始参数...>`；已处于沙盒时直接 `exec` 真实 pi，继承外层限制，不校验用于生成 profile 的 `PI_CODING_AGENT_DIR` 或读取 `sandbox.sb`，真实路径仍从系统账户的 `env` 文件读取。沙盒检测或启动失败直接失败。Linux 直接 `exec` 真实 pi，不要求 `PI_CODING_AGENT_DIR`。pi 不占用 Codex 并发槽位，不创建代理状态或日志。

## 锁和进程

```text
.codex-proxy/
  env
  env.example
  sandbox.sb
  state.lock
  state.json
  locks/
    admission.lock
    slot-0.lock
    slot-1.lock
    ...
  log.lock
  codex-proxy.log
  codex-proxy.log.1
  codex-app-server.log
  codex-app-server.log.1
  ...
```

调度锁保护统计和分配，每个运行任务持有一个独占槽位锁。槽位按需创建，统计全部历史槽位，因此降低上限也能正确计数。锁文件始终保留，是否占用由内核锁决定；运行期间不要删除锁文件或整个目录。

提交间隔检查在成功获取槽位之后执行，等待时继续占用槽位。每次提交只随机抽取一次 3–15（含两端）的整数秒数，然后获取独立的 `state.lock`，读取 `state.json` 中的 `last_codex_task_at`（秒级 Unix 时间戳）。若 `当前时间戳 - 上次提交时间戳 < 随机秒数`，等待剩余的秒数；状态文件首次不存在或尚无任务时间字段时直接放行。锁一直持有到真实 `exec` 子进程启动或 app-server 模型任务 RPC 发出，避免多个进程同时读取旧时间后一起提交。两种入口以及同一账户的所有代理进程共用这组文件；路径由账户的 `env` 文件统一配置，运行时 `HOME`、`USER`、`CODEX_HOME` 不会改变其位置。

可在 `.codex-proxy/env` 中分别配置状态锁和状态文件，例如指向已挂载的 macOS 内存文件系统：

```dotenv
CODEX_PROXY_STATE_LOCK_PATH=/Volumes/CodexProxyRuntime/state.lock
CODEX_PROXY_STATE_PATH=/Volumes/CodexProxyRuntime/state.json
```

两项独立生效；未配置或值为空时，各自使用原来的默认路径。非空值必须是绝对文件路径，且锁与状态文件不能指向同一文件（包括目录别名、大小写别名和硬链接）。父目录需要预先创建并保证当前系统账户可读写；目标文件不存在时在指定位置创建，父目录不存在、路径无效或访问失败时拒绝本次提交，不会静默回退。状态写入的临时文件始终位于目标 `state.json` 的父目录，因此可以将状态文件放到另一文件系统。配置、并发槽位和日志仍位于系统账户的 `.codex-proxy` 目录。内存文件系统在重启后丢失状态，需要重新挂载。

sleep 结束后，在提交 Codex 任务之前重新获取当前时间，将其秒级 Unix 时间戳写入 `state.json` 的 `last_codex_task_at`，不会使用等待前的时间。更新时保留其他状态字段，通过同目录临时文件原子替换写入，锁和状态文件权限均为 `0600`。启动或 RPC 写入失败可能保留本次放行时间，后续任务仍按该时间等待。状态损坏、锁或状态写入失败会拒绝本次提交；系统时钟回退时从当前时间等待完整随机间隔。app-server 的模型任务提交（包括已有槽位的追加请求及队列实际执行）都执行此检查，等待不阻塞查询、审批或取消。信号、客户端断开或取消会中止等待并清理相应锁；取消运行中任务的追加请求会保留原任务的槽位。

代理和原生 Codex 通过继承 FD 共享槽位锁。强杀代理后，原生 Codex 仍持锁；正常确认原生进程退出后，代理解锁，后台工具不再占用执行额度。原生进程必须保留该 FD。若代理被强杀且后台后代也继承锁，可能保守地延迟释放，直到这些进程关闭描述符。

stdio 直接透传，JSON stdout 不会混入代理日志。转发 `SIGINT`、`SIGTERM`、`SIGHUP`；非终端输入使用独立子进程组，终端输入保留原组。终端 Ctrl-C 可能同时由终端和代理递送。嵌套 `codex exec` 也占用额度，父任务等待子任务时需预留容量或等待超时。

代理只识别 `exec` / `e` 和 `app-server`，并跳过已知顶层选项的值，避免将 `--model exec` 误判为命令。未知选项不由代理拒绝；其他命令直接透传。

## 日志和退出码

日志固定为单行文本，没有格式配置项。每行依次为带明确时区的 UTC 时间（例如 `2026-10-26T12:51:45.168Z`）、事件和 `key=value` 字段；`user`、`status`、`method`、`error`、耗时及标识等常用字段优先，其余字段按名称稳定排序。字符串含空白、引号、反斜杠或控制字符时使用双引号 Go 转义，换行不会产生额外日志行；Unicode 保持可读，数组、对象等复杂值写成紧凑 JSON。示例：

```text
2026-10-26T12:51:45.168Z task_finished user=alice status=failed error="上游超时\n重试失败" run_ms=1200 request_id=8f2c pid=4242
```

记录调用标识、代理 PID、子进程 PID、等待/运行时长、退出码或信号，以及等待、启动、结束、超时、拒绝、取消和错误事件；强杀代理可能缺少结束日志。历史日志文件可能同时保留旧的 JSON Lines 记录和当前文本记录，按行首是否为 `{` 即可区分，`tail` 可直接阅读两种记录。

fake codex 的日志包含 `args` 数组；启动准备成功后记录实际传给原生 Codex 的参数，包含 macOS 自动补充或覆写的选项，准备失败时记录调用参数。`exec` / `e` 会剔除提示词，保留选项、已知选项值、子命令及会话 ID；未知选项后的独立值无法与提示词区分时省略，附着值原样记录。`app-server` 只记录固定的 `["app-server"]`，避免配置参数中的指令进入日志。其他命令完整记录参数。不记录 stdin 或 Codex 输出。

fake codex 的 app-server 详细日志写入 `codex-app-server.log`（原守护服务日志名为 `codex-app-server-proxy.log`），与 `codex-proxy.log` 使用相同日志大小、备份数量和跨进程日志锁。每条记录都包含 `user`、`args`、会话工作目录 `session_cwd`、代理 PID；原生进程启动后还包含 `child_pid`。两份日志使用同一个 `request_id`，可直接关联同一次调用。RPC 的 `rpc_id` 保留客户端标识，`rpc_sequence` 标识内部转发；任务从等待开始就有 `task_id`，每次提交另有 `submission_id`，并记录 `method`、`thread_id`、`turn_id`、`queue_id`、`process_handle` 及请求中的模型、工作目录等适用字段。

app-server 记录客户端 RPC 返回、任务接收、等待、入场、提交、开始、结束和槽位释放，以及队列变化、进程开始/退出、取消、超时、拒绝、错误和会话退出。不记录 `notification`、`rpc_received`、`rpc_started`，成功的代理内部 RPC 也不记录；客户端 RPC 的 `rpc_finished` 保留结果和耗时，失败或取消的内部 RPC 仍保留。通知继续原样转发，其中错误通知以 `error` 事件、`stage: "notification"` 记录。原生 RPC 错误记录 `error_code` 以及实际的 `error` 正文和 `error_data`/`error_object`，任何非 null 的 `error` 字段（包括字符串、数组或空对象）都按失败处理并保留原始 `error_payload`；通知错误覆盖 `params.error`、`thread/realtime/error` 的 `params.message` 和 `turn/completed` 的 `turn.error`。代理自身错误、启动/系统/解析失败、guardian 与槽位清理失败附带阶段和实际原因，本地 RPC 可返回这些具体原因；客户端断开时补记未完成任务和 RPC 的取消。时长单位均为毫秒：

| 字段 | 含义 |
| --- | --- |
| `wait_ms` | 任务截至当前事件的总等待时间；提交后保存最终等待时长 |
| `queue_wait_ms` | 入队至开始申请槽位的等待时间 |
| `slot_wait_ms` | 申请共享并发槽位的耗时 |
| `interval_wait_ms` | 提交间隔阶段的耗时，包括 state 锁等待及时间戳写入 |
| `run_ms` | 会话或 turn 的运行时长；`released` 中为首次提交至释放槽位；RPC 为转发至返回；进程为发送 spawn 至退出通知 |
| `held_ms` | 取得槽位至释放的总时长，包含提交前的准备和间隔等待 |

`waiting` 的 `stage` 区分 `slot` 和 `task_interval`；并发等待/入场记录 `limit`、`wait_timeout_ms`，会话启动记录 `idle_timeout_ms`。仅提取协议中的运行元数据，不记录提示词、目标文本、线程标题、工具输入、原始 RPC、输出、stderr 或环境变量；但原生错误响应、启动/系统/解析失败是排障所需的主要信息，其正文和附加 data 会写入日志，可能回显提示词等调用方文本。正常成功消息和流式 delta 仍不逐条记录。真实程序自行写入其 `CODEX_HOME` 的日志不受代理日志控制。

各类命令的日志还包含 `user` 字段，从运行时 `CODEX_HOME` 的 `.codex` 父目录名提取，例如 `/xxx/alice/.codex` 记录为 `alice`，允许路径带尾斜杠。这里只解析路径，不解析符号链接，不使用 `USER` 或 `HOME`；`CODEX_HOME` 缺失、不是绝对路径、末级目录不是 `.codex`、包含无效文本或无法提取用户目录名时记录空字符串。该字段不改变命令执行或代理配置、锁和日志的位置。

独立跨进程日志锁协调写入与滚动，每次重新打开当前文件，达到大小上限前滚动。默认保留当前日志及五个备份，`.1` 为最新备份。调整保留数量在下一次写入时生效；降低大小不会截断历史备份，它们随正常轮转淘汰。每次写入都重新读取大小和备份数量，配置读取或校验失败时使用默认值；单条记录按最终输出的实际字节数判断上限，超过时标记 `truncated` 并优先删除最大的可选字段。目录为 `0700`，新建锁及日志为 `0600`。

日志锁等待最长 100ms；写入失败向 stderr 诊断并跳过该条，不影响并发限制。

| 退出码 | 含义 |
| --- | --- |
| `1` | app-server 代理未启用，拒绝调用 |
| `2` | 配置、账户、锁或启动错误 |
| `124` | exec 等待执行槽位超时；app-server 使用 RPC 错误 `-32001` |
| `128 + signal` | 等待期间取消或真实进程被信号终止 |
| 其他 | 真实 Codex 正常退出码，或 macOS sandbox-exec 的失败状态 |

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

测试使用临时账户目录和编译出的原生模拟程序，不调用模型或写入真实用户的代理配置。覆盖跨进程并发、动态降额和超时、stdio/环境透传、信号及强杀后的锁生命周期、配置错误、日志滚动和多进程写入；提交间隔额外验证随机范围、差值等待、状态持久化、跨进程互斥、占槽顺序和取消清理；app-server 额外覆盖任务分类、共享额度、配置热更新、队列、goal、detached review、策略覆写、日志隐私和子进程清理。macOS 测试还覆盖实际文件隔离、符号链接访问拒绝和参数覆写；环境拒绝 sandbox-exec 时，文件隔离测试明确跳过，进程调度测试通过包内测试注入绕过启动器，生产入口仍要求沙箱成功启动。

真实原生链路的 unsubscribe 和控制 RPC 回归需要显式指定原生 Codex 二进制（未指定时跳过）：

```sh
CODEX_PROXY_NATIVE_BIN=/absolute/path/to/native/codex \
  go test ./internal/proxy -run '^TestNativeAppServer' -count=1 -v
```

该测试运行真实代理入口和原生 app-server，使用临时代理配置、隔离的 `CODEX_HOME` 以及两个本地 Responses HTTP 服务，不使用真实账号或线上模型。验证 A 第一轮 → 原生卸载 → 原 threadId 在 B 继续且携带第一轮历史、另一线程继续使用 A，以及运行中取消订阅后的成功/失败记账和槽位释放。常规回归同时覆盖真实请求转发、关闭通知透传、重复取消、恢复订阅、父子线程、队列保留，以及非任务请求绕过槽位和提交间隔等待。

原生控制回归还验证参数错误未被策略覆写掩盖、无活动 turn 或 turnId 不匹配时透传原生拒绝、运行中的 shell 与等待模型输入同时存在时真正执行中断，以及被拒绝的 goal 暂停不取消等待激活。模拟测试覆盖 goal 清除、realtime 停止的确认顺序、父子线程 shell 记账和普通/未知 RPC 的字段与错误透传。
