# codex-proxy

使用 Go 开发的 Codex CLI 拦截器，要求 **Go 1.23.4 或更高版本**，支持 macOS、Linux。生产代码只使用标准库。

- `codex exec`、`codex e` 及其子命令按同机、同用户共享并发额度，满额等待。
- `codex app-server` 直接失败，退出码 `1`，不启动真实程序。
- 其他命令透传给真实 Codex，不占用执行槽位。

## 构建及接入

```sh
./build.sh
```

脚本可从任意工作目录调用，自动创建项目下的 `bin/` 目录，输出二进制 `bin/codex`。

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

先通过系统账户信息确认目录（macOS 可用 `dscl . -read /Users/$(id -un) NFSHomeDirectory`，Linux 可用 `getent passwd "$(id -u)"`），创建 `.codex-proxy`，将 [env.example](env.example) 复制为其中的 `env` 并修改真实路径。请使用账户记录中的绝对目录，而非可能已修改的 `$HOME`。

```dotenv
CODEX_PROXY_REAL_BIN=/absolute/path/to/native/codex
CODEX_PROXY_MAX_CONCURRENCY=1
CODEX_PROXY_WAIT_TIMEOUT=10m
CODEX_PROXY_LOG_MAX_SIZE_MIB=10
CODEX_PROXY_LOG_MAX_BACKUPS=5
```

| 配置键 | 默认值 | 含义 |
| --- | --- | --- |
| `CODEX_PROXY_REAL_BIN` | 必填 | 真实原生 Codex 的绝对路径 |
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

## macOS 执行沙箱

macOS 的 `exec`、`e` 及其子命令使用 `/usr/bin/sandbox-exec` 启动真实原生 Codex。调用方必须设置绝对路径的运行时环境变量 `CODEX_HOME=/xxx/{用户}/.codex`，代理据此派生共享父目录 `/xxx` 和当前用户目录 `/xxx/{用户}`。这不会改变代理配置、锁或日志的系统账户目录。

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
```

当前用户目录允许读写，其他用户目录的读写及元数据访问被拒绝；共享父目录仅允许读取元数据，不允许列目录。规则不限制共享目录之外的路径。路径会解析已有祖先的符号链接（例如 `/tmp` 对应 `/private/tmp`），用户目录解析后必须仍是共享父目录的直接子目录；不接受共享父目录为 `/`。

真实 Codex 自动应用 `--skip-git-repo-check --sandbox danger-full-access`，覆盖原有 `-s` / `--sandbox` 并去重 skip-git 参数；会移除强制 workspace-write 的 `--approve-for-me`。其他参数、提示词及 `--` 后内容保留。macOS 会校验 exec 子命令的选项，以安全识别需要覆写的参数。

配置缺失或路径无效时返回 `2`，且不会占用执行槽位。`sandbox-exec` 启动或应用规则失败时返回失败状态，不回退到无沙箱执行。launcher 保留 PID 和继承的槽位 FD，信号转发及并发控制继续有效。Linux 和其他 Codex 命令按原方式执行。

## 锁和进程

```text
.codex-proxy/
  env
  locks/
    admission.lock
    slot-0.lock
    slot-1.lock
    ...
  log.lock
  codex-proxy.log
  codex-proxy.log.1
  ...
```

调度锁保护统计和分配，每个运行任务持有一个独占槽位锁。槽位按需创建，统计全部历史槽位，因此降低上限也能正确计数。锁文件始终保留，是否占用由内核锁决定；运行期间不要删除锁文件或整个目录。

代理和原生 Codex 通过继承 FD 共享槽位锁。强杀代理后，原生 Codex 仍持锁；正常确认原生进程退出后，代理解锁，后台工具不再占用执行额度。原生进程必须保留该 FD。若代理被强杀且后台后代也继承锁，可能保守地延迟释放，直到这些进程关闭描述符。

stdio 直接透传，JSON stdout 不会混入代理日志。转发 `SIGINT`、`SIGTERM`、`SIGHUP`；非终端输入使用独立子进程组，终端输入保留原组。终端 Ctrl-C 可能同时由终端和代理递送。嵌套 `codex exec` 也占用额度，父任务等待子任务时需预留容量或等待超时。

命令解析匹配 Codex CLI 0.155.0 的顶层规则，处理选项值、别名和 `--`。未知顶层选项会报错，升级 CLI 后可能需要更新代理；识别到子命令后，其参数原样传递。

## 日志和退出码

JSON Lines 日志记录 UTC 时间、调用标识、代理 PID、子进程 PID、等待/运行时长、退出码或信号，以及等待、启动、结束、超时、拒绝、取消和错误事件。不记录提示词或 Codex 输出。强杀代理可能缺少结束日志。

独立跨进程日志锁协调写入与滚动，每次重新打开当前文件，达到大小上限前滚动。默认保留当前日志及五个备份，`.1` 为最新备份。调整保留数量在下一次写入时生效；降低大小不会截断历史备份，它们随正常轮转淘汰。过大的单条记录标记 `truncated` 并删除超长字段。目录为 `0700`，新建锁及日志为 `0600`。

日志锁等待最长 100ms；写入失败向 stderr 诊断并跳过该条，不影响并发限制。

| 退出码 | 含义 |
| --- | --- |
| `1` | 拒绝 `app-server` |
| `2` | 配置、账户、锁或启动错误 |
| `124` | 等待执行槽位超时 |
| `128 + signal` | 等待期间取消或真实进程被信号终止 |
| 其他 | 真实 Codex 正常退出码，或 macOS sandbox-exec 的失败状态 |

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

测试使用临时账户目录和编译出的原生模拟程序，不调用模型或写入真实用户的代理配置。覆盖跨进程并发、动态降额和超时、stdio/环境透传、信号及强杀后的锁生命周期、配置错误、日志滚动和多进程写入。macOS 测试还覆盖实际文件隔离、符号链接访问拒绝和参数覆写；运行环境必须允许 sandbox-exec 应用沙箱策略。
