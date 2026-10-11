# codex-proxy

使用 Go 开发的 Codex CLI 拦截器，要求 **Go 1.23.4 或更高版本**，支持 macOS、Linux。生产代码只使用标准库。

- `codex exec`、`codex e` 及其子命令按同机、同用户共享并发额度，满额等待。
- `codex app-server` 默认拒绝，退出码 `1`；启用配置后经 stdio 代理真实 app-server，执行模型任务时与 `exec` 共享并发额度。
- 其他命令透传给真实 Codex，不占用执行槽位。

## 构建及接入

```sh
./build.sh
```

脚本可从任意工作目录调用，自动创建项目下的 `bin/` 目录，输出 `bin/codex` 和 `bin/codex-app-server-proxy`。两个二进制必须安装在同一目录。构建成功后，在当前 UID 的系统账户主目录下创建 `.codex-proxy/`（权限 `0700`），并将 [env.example](env.example) 复制为 `.codex-proxy/env.example`（新建文件权限 `0600`）；已有模板会保留。

Linux 必须设置 `CGO_ENABLED=1`（脚本会自动设置），通过 libc 查询系统账户（支持 NSS）；两个平台都不要使用 `osusergo` 构建标签。Linux 关闭 cgo 的构建会在运行时明确失败，不会使用 `HOME` 回退。macOS 可关闭 cgo。Linux 构建需要 C 编译器及目标系统的 libc。

将编译产物所在目录放在 `PATH` 最前面。例如将固定的绝对路径写入正常的 shell 初始化配置：

```sh
export PATH="/absolute/path/to/codex-proxy/bin:$PATH"
```

保留真实 Codex 安装，不要用代理覆盖原始二进制。真实路径不会再通过 `PATH` 查找，避免递归调用。

## 配置

代理按当前 UID 查询系统账户数据库，从账户记录取得原始用户目录，固定读取：

```text
<系统账户目录>/.codex-proxy/env
```

运行时修改 `HOME`、`USER`、`CODEX_HOME` 不会改变代理配置、锁或日志位置；真实 Codex 继续使用调用方传入的环境与工作目录。账户查询失败时返回配置错误。

运行 `./build.sh` 后，将系统账户目录下的 `.codex-proxy/env.example` 复制为同目录的 `env`，并修改真实路径。脚本不会创建或覆盖实际配置 `env`。也可手动创建 `.codex-proxy`，将项目中的 [env.example](env.example) 复制为其中的 `env`。请使用账户记录中的绝对目录，而非可能已修改的 `$HOME`。

```dotenv
CODEX_PROXY_REAL_BIN=/absolute/path/to/native/codex
CODEX_PROXY_APP_SERVER_ENABLED=false
CODEX_PROXY_MAX_CONCURRENCY=1
CODEX_PROXY_WAIT_TIMEOUT=10m
CODEX_PROXY_LOG_MAX_SIZE_MIB=10
CODEX_PROXY_LOG_MAX_BACKUPS=5
```

| 配置键 | 默认值 | 含义 |
| --- | --- | --- |
| `CODEX_PROXY_REAL_BIN` | 必填 | 真实原生 Codex 的绝对路径 |
| `CODEX_PROXY_APP_SERVER_ENABLED` | `false` | 设为 `true` 启用 app-server 代理；未配置或 `false` 保持拒绝 |
| `CODEX_PROXY_MAX_CONCURRENCY` | `1` | 正整数，同时执行的上限 |
| `CODEX_PROXY_WAIT_TIMEOUT` | `10m` | 排队超时，支持 `30s`、`10m`；`0` 无限等待 |
| `CODEX_PROXY_LOG_MAX_SIZE_MIB` | `10` | 每个日志文件的上限，1 MiB = 1,048,576 字节 |
| `CODEX_PROXY_LOG_MAX_BACKUPS` | `5` | 非负整数，备份数量，不含当前日志；`0` 不保留备份 |

支持空行、`#` 注释、可选 `export` 前缀、单引号或双引号，以及空白后的行尾注释。双引号支持 Go 字符串转义。不会执行 shell、展开 `$VAR`、`~` 或命令替换。重复键、未知键、无效值会报错。进程中同名环境变量不会覆盖文件配置。

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

提高上限后，等待任务可以进入；降低上限时，已有任务继续运行，直到占用数量小于新上限才放行。超时从最初开始等待计算，修改配置不会重置计时；超时只限制入场等待，不限制运行时间。不保证排队顺序。

## app-server 代理

在系统账户目录的 `.codex-proxy/env` 中显式配置以下开关，才会启用代理：

```dotenv
CODEX_PROXY_APP_SERVER_ENABLED=true
```

默认关闭。配置文件缺失、开关未配置或设为 `false` 时，fake codex 拒绝所有被拦截的 app-server 命令（包括 `--help` 和协议生成子命令），保持原来的提示和退出码 `1`，不启动真实程序、不占槽位。开关只读取该文件，进程环境变量无法覆盖；无效布尔值返回配置错误 `2`。服务入口也独立检查开关。修改开关影响新连接，已有连接继续运行，并发和等待参数仍在每次任务入场时重新读取。

每个客户端连接使用以下进程链路，不需要后台守护服务或 socket 文件：

```text
客户端 ⇄ fake codex ⇄ codex-app-server-proxy ⇄ 真实 codex app-server
                  stdio                    stdio
```

fake codex 将原始参数、工作目录和环境封装为私有启动请求，作为第一行发给服务；服务消费此行，后续内容按逐行 JSON-RPC 转发。请求 ID 会在内部连接上重新分配，在返回客户端时恢复，避免与代理自己的设置请求冲突。真实程序的 stderr 直接转发到调用方 stderr。服务只支持默认 stdio 或 `--listen stdio://`，拒绝 WebSocket、daemon、proxy 子命令和远程控制入口，避免任务绕过共享锁。`--help`、`generate-ts`、`generate-json-schema` 直接透传，不占槽位。

使用方式与原命令相同，客户端仍连接 fake codex 的 stdin/stdout：

```sh
CODEX_HOME=/xxx/alice/.codex codex app-server
```

已按 Codex 0.162.1 的协议验证。服务需要真实程序支持 `thread/settings/update`；在内部 `initialize` 请求中启用 `experimentalApi`，以便普通客户端也能应用该设置。线程创建、恢复、fork 后，以及任务首次放行前，强制设置 `approvalPolicy: "never"` 和以下策略：

```json
{"sandboxPolicy":{"type":"externalSandbox","networkAccess":"enabled"}}
```

线程创建接口的 `sandbox` 参数只能使用枚举，因此先以 `danger-full-access` 创建线程，再通过内部设置请求应用 externalSandbox。任务及设置请求中的相冲突策略会被覆写；无法应用策略时拒绝任务。macOS 外层继续使用下文的 `sandbox-exec` 文件隔离规则。

| 协议操作 | 并发控制 |
| --- | --- |
| `turn/start` | 新任务获取槽位；运行中的同一 turn 补充输入沿用原槽位 |
| `review/start`、`thread/compact/start` | 持锁直到相应 turn 完成、失败或中断；detached review 独立占用槽位 |
| `thread/approveGuardianDeniedAction` | 恢复任务前获取槽位，已运行任务沿用原槽位 |
| `thread/prediction/request` | 获取槽位，结果通知或错误后释放；真实程序不支持时透传其错误 |
| `thread/realtime/start` | 获取槽位，持续至 `thread/realtime/closed`；追加输入不重复获取 |
| `thread/goal/set` 激活或恢复 goal | 整个自动续跑链持有一个槽位，暂停、结束或中断且当前 turn 结束后释放 |
| `thread/queue/add`、list、update、delete、reorder | 操作队列不占槽位，每条消息实际开始执行前重新获取槽位 |
| `turn/steer`、审批或工具回应、查询、登录、线程元数据、shell/command/process/fs 操作 | 不额外占槽位 |

队列由代理在内存中管理，通过 `turn/start` 执行每条消息，避免真实 app-server 的自动出队绕过 admission。每个线程最多保存 1,024 条待执行消息，连接内队列输入总量最多 32 MiB；单条协议消息最多 32 MiB，同时等待槽位的任务最多 128 个。入队后空闲线程自动执行；超时、失败、中断后暂停队列，可通过 `thread/queue/start` 恢复。客户端断开时队列随服务结束，不持久化。fork 强制 `deferGoalContinuation: true`，复制的 goal 由下次显式 turn 或 goal 恢复请求启动；代理在新 turn 放行前查询 goal，确保复制或恢复的自动续跑链继续持锁。

每次新任务申请及每次等待重试都重新读取同一个 `.codex-proxy/env`，使用与 exec 完全相同的文件锁。等待不会阻塞协议查询或审批回应。等待超时返回 JSON-RPC 错误 `-32001`，取消返回 `-32800`，配置、锁或策略设置失败返回 `-32000`；服务继续处理其他请求。等待期间的 `turn/interrupt` 或 `$/cancelRequest` 可取消尚未开始的任务。

客户端 EOF、SIGINT、SIGTERM、SIGHUP 时，服务取消等待，终止真实 app-server 的进程组；500ms 后仍未退出则 SIGKILL，并在回收进程后释放锁。每个持锁任务另有一个继承槽位 FD 的守护进程：服务被 SIGKILL 时，控制管道断开，守护进程终止真实程序，确认进程组消失后才释放锁。若系统迟迟不回收孤儿进程，槽位会保守地保持占用。

## macOS 执行沙箱

macOS 的 `exec`、`e` 及其子命令和 app-server 服务使用 `/usr/bin/sandbox-exec` 启动真实原生 Codex。调用方必须设置绝对路径的运行时环境变量 `CODEX_HOME=/xxx/{用户}/.codex`，代理据此派生共享父目录 `/xxx` 和当前用户目录 `/xxx/{用户}`。这不会改变代理配置、锁或日志的系统账户目录。

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
```

当前用户目录允许读写，其他用户目录的读写及元数据访问被拒绝；共享父目录仅允许读取元数据，不允许列目录。固定规则额外允许 `/xxx` 及其子路径的元数据查询，即使位于共享目录的其他用户区域；此规则本身只放开元数据权限。规则不限制共享目录之外的路径。路径会解析已有祖先的符号链接（例如 `/tmp` 对应 `/private/tmp`），用户目录解析后必须仍是共享父目录的直接子目录；不接受共享父目录为 `/`。

真实 Codex 自动应用 `--skip-git-repo-check --dangerously-bypass-approvals-and-sandbox`：已有对应选项时不重复添加；已有 `-s` / `--sandbox` 时移除该选项及其值，包括附着值和 `=` 写法。若被移除的选项紧接 `--image` / `-i` 的独立图片值，图片参数会改为逐个附着值（例如 `--image=a.png --image=b.png`），保留图片列表与后续提示词的边界。其他参数（包括 `--experimental-json`、未知选项）、提示词及 `--` 后内容原样保留，参数校验交给真实 Codex。`--dangerously-bypass-approvals-and-sandbox` 跳过 Codex 自身的审批和沙箱，外层 `sandbox-exec` 继续应用上述文件访问规则。

配置缺失或路径无效时返回 `2`，且不会占用执行槽位。`sandbox-exec` 启动或应用规则失败时返回失败状态，不回退到无沙箱执行。launcher 保留 PID 和继承的槽位 FD，信号转发及并发控制继续有效。Linux 和其他 Codex 命令按原方式执行。

## 锁和进程

```text
.codex-proxy/
  env
  env.example
  locks/
    admission.lock
    slot-0.lock
    slot-1.lock
    ...
  log.lock
  codex-proxy.log
  codex-proxy.log.1
  codex-app-server-proxy.log
  codex-app-server-proxy.log.1
  ...
```

调度锁保护统计和分配，每个运行任务持有一个独占槽位锁。槽位按需创建，统计全部历史槽位，因此降低上限也能正确计数。锁文件始终保留，是否占用由内核锁决定；运行期间不要删除锁文件或整个目录。

代理和原生 Codex 通过继承 FD 共享槽位锁。强杀代理后，原生 Codex 仍持锁；正常确认原生进程退出后，代理解锁，后台工具不再占用执行额度。原生进程必须保留该 FD。若代理被强杀且后台后代也继承锁，可能保守地延迟释放，直到这些进程关闭描述符。

stdio 直接透传，JSON stdout 不会混入代理日志。转发 `SIGINT`、`SIGTERM`、`SIGHUP`；非终端输入使用独立子进程组，终端输入保留原组。终端 Ctrl-C 可能同时由终端和代理递送。嵌套 `codex exec` 也占用额度，父任务等待子任务时需预留容量或等待超时。

代理只识别 `exec` / `e` 和 `app-server`，并跳过已知顶层选项的值，避免将 `--model exec` 误判为命令。未知选项不由代理拒绝；其他命令直接透传。

## 日志和退出码

JSON Lines 日志记录 UTC 时间、调用标识、代理 PID、子进程 PID、等待/运行时长、退出码或信号，以及等待、启动、结束、超时、拒绝、取消和错误事件。强杀代理可能缺少结束日志。

fake codex 的日志包含 `args` 数组；启动准备成功后记录实际传给原生 Codex 的参数，包含 macOS 自动补充或覆写的选项，准备失败时记录调用参数。`exec` / `e` 会剔除提示词，保留选项、已知选项值、子命令及会话 ID；未知选项后的独立值无法与提示词区分时省略，附着值原样记录。`app-server` 只记录固定的 `["app-server"]`，避免配置参数中的指令进入日志。其他命令完整记录参数。不记录 stdin 或 Codex 输出。

app-server 服务独立写入 `codex-app-server-proxy.log`，使用相同日志大小、备份数量和跨进程日志锁。只记录事件、代理生成的任务标识、固定方法名、PID、并发上限、等待/运行时长、状态及退出码；不记录调用参数、环境、提示词、目标文本、线程标题、工具输入、原始 RPC、输出、stderr 或真实程序的错误文本。真实程序自行写入其 `CODEX_HOME` 的日志不受代理日志控制。

各类命令的日志还包含 `user` 字段，从运行时 `CODEX_HOME` 的 `.codex` 父目录名提取，例如 `/xxx/alice/.codex` 记录为 `alice`，允许路径带尾斜杠。这里只解析路径，不解析符号链接，不使用 `USER` 或 `HOME`；`CODEX_HOME` 缺失、不是绝对路径、末级目录不是 `.codex`、包含无效文本或无法提取用户目录名时记录空字符串。该字段不改变命令执行或代理配置、锁和日志的位置。

独立跨进程日志锁协调写入与滚动，每次重新打开当前文件，达到大小上限前滚动。默认保留当前日志及五个备份，`.1` 为最新备份。调整保留数量在下一次写入时生效；降低大小不会截断历史备份，它们随正常轮转淘汰。过大的单条记录标记 `truncated` 并删除超长字段。目录为 `0700`，新建锁及日志为 `0600`。

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

测试使用临时账户目录和编译出的原生模拟程序，不调用模型或写入真实用户的代理配置。覆盖跨进程并发、动态降额和超时、stdio/环境透传、信号及强杀后的锁生命周期、配置错误、日志滚动和多进程写入；app-server 额外覆盖任务分类、共享额度、配置热更新、队列、goal、detached review、策略覆写、日志隐私和子进程清理。macOS 测试还覆盖实际文件隔离、符号链接访问拒绝和参数覆写；环境拒绝 sandbox-exec 时，文件隔离测试明确跳过，进程调度测试通过包内测试注入绕过启动器，生产入口仍要求沙箱成功启动。
