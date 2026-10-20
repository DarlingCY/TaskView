# TaskView 📋

专为开发者与 AI Agent 协作场景打造的**轻量本地任务与优化项跟踪系统**。

在与 Agent 对话编程的过程中，Agent 经常会扫描出性能瓶颈、潜在 Bug、重构建议或待办项，但往往无法在单次对话中全部消化。**TaskView** 采用纯 Go 编写为独立单二进制（Single Binary）工具，通过极简 CLI、本地集中式 SQLite 存储以及内嵌 Web 看板，让你和 Agent 可以随时记录、沉淀、流转这些任务。

---

## 🌟 核心特性

- **独立单二进制（Single Binary）**：基于 Go 原生开发，静态编译（`CGO_ENABLED=0`），内置 `modernc.org/sqlite` 纯 Go 数据库引擎。分发出的单一可执行文件运行**无需安装 Node.js、Go 运行时环境，也无需系统预装任何 sqlite3 动态库**。
- **内嵌 Web 看板与按需后台启动**：`taskview ui` 启动或复用后台服务，健康检查通过后打开浏览器并返回命令提示符；可用 `server status` 检查状态、`server stop` 停止服务。进程异常退出后，下次执行 `ui` 或 `server start` 时重新启动。
  > **注意**：内置前端 SPA 目前仍通过外部 CDN 引入 Tailwind CSS 与 Google Fonts。浏览器访问时会向外部网络发起资源请求，任何未缓存的访问均需要网络连接；系统目前并非严格的网络隔离环境，离线静态资源尚未内置，待后续阶段实现资源本地化（Vendoring）。
- **用户全局集中存储**：数据集中保存在 `~/.taskview/tasks.db`（可通过环境变量 `TASKVIEW_HOME` 自定义），CLI 命令直连 SQLite，默认记录并按当前工作目录（CWD，或通过 `--project` 显式指定）进行索引（不会向上自动探测代码仓库根目录），完全兼容历史数据表结构与 ID，无需数据迁移。
- **本地数据存储与回环接口**：任务数据保存在用户主目录下，应用本身无任何内置远程同步或团队共享机制。Web 服务仅监听本地回环地址（127.0.0.1 / localhost），但回环绑定并不等同于身份认证，也无法防御本机其他应用的访问；同时因前端加载第三方 CDN 脚本与字体，应用并非严格的网络隔离或绝对私密。
- **可选系统安装与 Windows 登录自启动**：支持通过 `taskview install` 将自身复制至受管目录（`~/.taskview/bin/taskview.exe`），或通过 `taskview install --autostart` 显式注册 Windows 任务计划程序实现登录后台自启，完全由用户显式选择，绝不隐式修改系统。
- **专为 Agent 定制的数据模型**：
  - 关联代码文件路径（`--file`）与行号区间（`--lines`）
  - 代码片段（`--code`）
  - Agent 建议与记录更新（`--notes` 覆盖备注、`--append-notes` 追加记录）
  - 四级优先级（`low`、`medium`、`high`、`urgent`）
  - 四列看板流转状态（`backlog`、`todo`、`in_progress`、`done`）
- **现代化 Web 看板**：
  - 看板（Kanban）与列表（List）视图自由切换
  - 原生卡片拖拽流转状态（乐观更新 + 实时持久化）
  - 代码上下文抽屉查看器（带行号显示与指定行区间高亮，非语法高亮）
  - 全局搜索、标签/优先级过滤、深浅色主题切换
- **Agent Skill 开箱即用（多平台自包含 Skill）**：提供自包含的通用 Agent 技能包（位于 [`skills/taskview`](skills/taskview)），内置 Windows、Linux 与 macOS 独立可执行程序。主流 AI 编程助手导入后即可在目标工程中直接调用，无需用户单独安装 Go/Node/SQLite 运行时或配置系统 PATH。

---

## 🚀 快速上手

构建生成或下载的单二进制文件位于 `dist/` 目录（例如 `dist/taskview.exe` 或 `dist/taskview`）。在配置系统环境变量 `PATH` 之前，可以直接通过相对路径直接执行；配置 `PATH` 后即可在任意工作目录直接运行 `taskview`。

运行 `taskview` 或 `.\dist\taskview.exe`（无参数）即可输出完整使用帮助。

### 1. 启动 Web 看板

在终端中执行以下命令，会自动在本地回环地址确保后台服务运行，并调起默认浏览器访问 Web 看板。启动确认后命令返回，终端可继续使用，服务在后台持续常驻：

**Windows PowerShell（未配置 PATH 时直接调用 dist 产物）：**
```powershell
# 启动/复用后台 Web 界面并打开浏览器（启动确认后命令返回，终端可继续使用）
.\dist\taskview.exe ui

# 仅确保后台服务运行，不自动打开浏览器
.\dist\taskview.exe ui --no-open

# 查看后台服务状态（只读检查，不创建目录或访问 DB）
.\dist\taskview.exe server status

# 停止后台服务
.\dist\taskview.exe server stop
```

**POSIX (Bash / Zsh)：**
```bash
# 启动/复用后台 Web 界面并打开浏览器（启动确认后命令返回，终端可继续使用）
./dist/taskview ui

# 仅确保后台服务运行，不自动打开浏览器
./dist/taskview ui --no-open

# 查看后台服务状态
./dist/taskview server status

# 停止后台服务
./dist/taskview server stop
```

> **提示**：若已将 `dist` 所在目录添加至系统 `PATH`（或执行了可选的 `taskview install` 并配置 PATH），上述命令可直接简写为 `taskview ui`、`taskview server status` 等。

---

## 💻 命令行（CLI）用法

TaskView 提供了直观直接的命令行交互。所有数据读写（`add`, `list`, `get`, `update`, `delete`, `stats`）均**直接基于本地 SQLite 进行**，无需依赖后台 Web 服务，也无需安装或配置自启。

> **⚠️ 重要机制：工作目录（CWD）与工程作用域**
> - **直接 CLI 命令**：严格记录执行命令时的**当前工作目录**（CWD），或通过 `--project <path>` 显式指定路径（不会向上自动探测仓库根目录）。
> - **Web 看板**：看板初始载入所有项目的任务总览（All Projects）。在 Web 界面创建新任务时，若未显式选择工程，则默认归属后台服务启动时所在的工作目录（Server CWD）；在界面中显式选择工程则以选中工程为准。调用 `taskview ui` 复用已有服务时**不会改变**已有服务的启动目录。

### 1. 创建任务 / 优化项 (`add`)

**Windows PowerShell 示例：**
```powershell
# 极简创建
taskview add "重构 Auth 中间件的 Token 缓存"

# 附带完整代码上下文与分析建议（单行执行）
taskview add "重构 Auth 中间件的 Token 缓存" --desc "高并发场景容易耗尽连接池" --file "src/middleware/auth.go" --lines "42-65" --priority "high" --status "todo" --notes "建议引入 LRU 内存缓存" --tags "perf,auth"

# PowerShell 续行语法（使用反引号 `）
taskview add "重构 Auth 中间件的 Token 缓存" `
  --desc "高并发场景容易耗尽连接池" `
  --file "src/middleware/auth.go" `
  --lines "42-65" `
  --code "token := db.Query(ctx, sql)" `
  --priority "high" `
  --status "todo" `
  --notes "建议引入 LRU 内存缓存" `
  --tags "perf,auth"
```

**POSIX (Bash / Zsh) 示例：**
```bash
taskview add "重构 Auth 中间件的 Token 缓存" \
  --desc "高并发场景容易耗尽连接池" \
  --file "src/middleware/auth.go" \
  --lines "42-65" \
  --code "token := db.Query(ctx, sql)" \
  --priority "high" \
  --status "todo" \
  --notes "建议引入 LRU 内存缓存" \
  --tags "perf,auth"
```

### 2. 查看任务列表 (`list`)

```powershell
# 查看当前工作目录关联的任务
taskview list

# 查看跨所有项目的全部任务
taskview list --all

# 按状态或优先级过滤
taskview list --status todo --priority high

# 关键词搜索与排序（updated | created | priority | title）
taskview list --search "auth" --sort priority
```

### 3. 查看任务详情 (`get`)

```powershell
taskview get <task_id>
```

### 4. 更新任务状态与记录 (`update`)

```powershell
# 更新任务流转状态
taskview update <task_id> --status in_progress

# 标记完成（自动记录 closed_at 闭环时间戳）
taskview update <task_id> --status done

# 覆盖备注信息 (--notes)
taskview update <task_id> --notes "重新评估方案：改用 Redis 集中缓存"

# 追加备注进展 (--append-notes，保留历史并追加)
taskview update <task_id> --append-notes "已完成压力测试，QPS 提升 40%"
```

### 5. 统计与概览 (`stats`)

```powershell
# 查看当前工作目录任务统计（默认仅统计当前 CWD；跨工程全局统计请使用 --all）
taskview stats

# 查看跨工程全局统计
taskview stats --all
```

### 6. 删除任务 (`delete`)

```powershell
taskview delete <task_id>
```

---

## 🤖 导入 Agent Skill（随包附带程序）

TaskView 提供了通用的自包含 Agent 技能包（Agent Skill），使主流 AI 编程助手（如 OpenCode、Claude Code 及支持 Agent Skills 规范的宿主）能够在目标工程中直接调用 TaskView 记录与管理任务。技能包内直接附带了预编译的多平台独立程序，**导入后即可直接使用，终端用户无需单独安装 Go/Node/SQLite 运行时，亦无需将程序加入系统 PATH**。

### 导入方式与目录

在构建后，[`skills/taskview`](skills/taskview) 本身即为包含 Windows、Linux 与 macOS 平台可执行文件的完整技能包（位于 `skills/taskview/scripts/`）。用户或宿主软件可直接通过宿主提供的技能机制配置该目录，或将整个 `taskview` 文件夹复制至宿主识别的技能目录（形成 `<skills-dir>/taskview/SKILL.md`）并重新加载生效。

- **完整导入与配置指引**：详见 [TaskView Skill 配置与导入指南](skills/taskview/README.md)。

> **维护者构建命令**：  
> 若从源码修改了代码或更新了技能文档，维护者可执行以下脚本同步生成 `skills/taskview/scripts/` 内的 3 个平台可执行程序（需要 Go 1.26.5+ 编译环境）：  
> ```powershell
> pwsh -File ./scripts/build-skills.ps1
> ```

---

## 🖥️ 服务与后台生命周期管理 (`server`)

TaskView 支持按需启动后台 Web 服务，无需保持启动它的终端窗口。

### 常用命令

```powershell
# 启动或复用后台服务并调起浏览器（启动确认后命令返回，终端可继续使用）
taskview ui

# 启动后台服务（不打开浏览器）
taskview server start

# 指定端口启动后台服务（默认 3333）
taskview server start --port 8080

# 只读查看后台服务状态与监听 URL（纯只读健康检查：不初始化 DB，不新建任何文件或目录）
taskview server status

# 安全且幂等停止后台服务（通过私有控制 Token 认证停机）
taskview server stop
```

### 高级/内部命令

```powershell
# 前台直接运行 Web 服务（内部子进程入口，也可用于前台调试）
taskview server run --port 3333 --data-dir "C:\Users\<Username>\.taskview"
```

登录自启使用 `server start --data-dir <绝对路径>`：启动器确认后台服务可用后退出，不保留常驻终端窗口。

### 端口绑定与实例复用机制

1. **自动端口顺延**：默认绑定 `3333` 端口。若该端口被其他非 TaskView 应用占用，服务会自动依次向上尝试后续最多 10 个端口（例如 3334 ~ 3343）。
2. **同数据目录实例复用**：相同数据目录（默认 `~/.taskview` 或 `$TASKVIEW_HOME`）下已有健康服务时，`ui` 和 `server start` 复用已记录的实际端口，即使本次指定了不同的 `--port`。只有 `ui` 默认打开浏览器，`server start` 不打开浏览器。
3. **精准身份认证防误杀**：停止服务或健康检测基于 `server.json` 中的控制 Token 交互验证，绝不会基于随机 PID 盲目终止系统进程。

### 运行时文件与安全凭据说明

- `~/.taskview/server.json`：存放服务 PID、监听端口及**私有控制 Token**。
  > ⚠️ **安全警告**：`server.json` 中的 Token 为本地服务控制凭据，**严禁向外界分享、打印或暴露其内容**。
- `~/.taskview/server.log`：记录服务启动、停止和错误日志，不主动记录控制 Token。

---

## 📦 系统安装与 Windows 登录自启动（可选） (`install` / `uninstall`)

TaskView 支持将自身复制到用户主目录的标准位置，并支持可选配置 Windows 登录自启。该功能完全遵循用户显式 Opt-in，绝不隐式修改系统。

### 1. 安装到受管目录 (`install`)

```powershell
# 将当前 taskview 可执行文件复制到 ~/.taskview/bin/taskview.exe
taskview install
```

- **稳定存放**：可执行文件统一安装至 `~/.taskview/bin/taskview.exe`（或 `$TASKVIEW_HOME/bin/taskview.exe`）。安装位置稳定，仅在显式执行 install 时更新。
- **不修改 PATH**：安装命令**不会自动修改系统 PATH** 环境变量，用户可按需决定是否将受管目录添加至 PATH。
- **不自动启动**：安装完成后不会自动启动后台服务。
- **文件替换与服务更新**：在 Windows 平台下，正在执行的文件存在系统锁限制；升级覆盖时可能需要停止受管服务后再进行替换，若发生错误会给出明确提示。

### 2. 启用 Windows 登录后台自启动 (`install --autostart`)

```powershell
# 安装并注册 Windows 任务计划程序（用户登录时自动后台拉起服务）
taskview install --autostart
```

- **用户级显式注册**：在当前用户（Current User）作用域下注册 Windows 任务计划程序（Task Scheduler），用户登录后启动本地服务，不自动打开浏览器。
- **自启配置继承**：一旦配置了 `--autostart`，后续执行普通的 `taskview install`（如版本升级）会**自动继承保留**先前的登录自启动配置，无需重复指定参数。
- **权限与排查指引**：在部分受限策略的 Windows 环境中注册任务计划程序可能需要额外权限；若遇到注册错误，命令会输出清晰可行的错误指引，**绝不进行任何静默或未经许可的自动 UAC 提权**。
- **跨平台说明**：`--autostart` 为 Windows 独占特性；在 Linux / macOS 系统上允许执行基础 `taskview install`，但使用 `--autostart` 会明确提示不受支持。
- **硬件冷重启验证声明**：自启注册通过 Windows 任务计划程序配置完成，实际物理开机/冷重启登录触发事件需用户在日常实际使用中验证。

### 3. 卸载与清理 (`uninstall`)

```powershell
# 卸载 TaskView
taskview uninstall
```

- **安全注销**：自动验证并安全停止正在运行的后台服务。
- **清理注册与程序**：注销 Windows 任务计划程序中的自启任务，并删除受管可执行文件（`~/.taskview/bin/taskview.exe`；在 Windows 下若文件仍被短暂占用，可能在命令退出后延迟完成删除）。
- **保留用户数据**：**默认保留**用户数据库 `tasks.db` 与日志 `server.log`，绝不静默清除历史任务数据。

---

## 🌐 环境变量 `TASKVIEW_HOME` 独立数据目录

TaskView 默认使用 `~/.taskview` 作为数据与配置根目录。通过设置环境变量 `TASKVIEW_HOME`，您可以指定自定义路径以实现同用户下的独立数据目录分离（注：同一系统用户下不同进程间的安全防护不在隔离范围内）：

```powershell
# Windows PowerShell 设置独立数据目录
$env:TASKVIEW_HOME = "D:\MyWorkspace\.taskview_dev"

# 执行命令将完全作用于该数据目录
taskview install --autostart
taskview ui
```

在此独立数据目录下：
- **数据库**：位于 `$TASKVIEW_HOME/tasks.db`
- **受管二进制**：位于 `$TASKVIEW_HOME/bin/taskview.exe`
- **运行时凭据与日志**：位于 `$TASKVIEW_HOME/server.json` 与 `$TASKVIEW_HOME/server.log`
- **计划任务分离**：Windows 任务计划程序会根据数据目录的绝对路径哈希生成独立的任务名称，不同目录互不冲突。

---

## 🔄 从 Node 旧版本平滑迁移

如果此前使用过基于 Node.js 实现的旧版 TaskView：

1. **CLI 命令平替**：旧版命令（如 `node bin/taskview.js <command>`）已完全由 Go 单二进制（`taskview <command>` 或 `.\dist\taskview.exe <command>`）替代，常用 CLI 子命令与参数完全一致。
2. **零破坏性数据继承**：数据继续使用相同的存储路径（默认 `~/.taskview/tasks.db`，或环境变量 `TASKVIEW_HOME`），表结构、字段别名及任务 ID 规范完全兼容，旧版创建的任务无需任何迁移操作即可直接沿用。
3. **关闭旧版服务**：启动新版 UI 之前，若此前已有旧版 Node Web 进程在后台运行，请先终止旧服务进程，释放 3333 端口以防冲突。

---

## 🛠️ 编译与开发构建

### 环境要求

- **构建端**：需要安装 Go（最低要求 Go 1.26.5，见 `go.mod`），并具备网络连接以下载外部依赖模块（`modernc.org/sqlite` 等）。
- **运行端**：分发后的独立可执行文件运行**无需 Go、Node.js 或外部 SQLite 运行时**。

### 运行测试与静态检查

```powershell
# 运行单元测试
go test ./...

# 静态代码检查
go vet ./...
```

Windows 计划任务注册集成测试需要显式启用。它只创建唯一命名的临时任务，并在测试结束后删除，不配置日常使用的自启动：

```powershell
$env:TASKVIEW_TEST_SCHEDULER = "1"
go test -count=1 ./...
Remove-Item Env:\TASKVIEW_TEST_SCHEDULER
```

### 编译单二进制可执行文件

构建时将生成独立可执行文件至 `dist/` 目录：

**Windows PowerShell:**
```powershell
# 设置纯 Go 静态编译（无需 C 编译器工具链，输出至 dist/ 目录）
$env:CGO_ENABLED="0"
go build -trimpath -ldflags="-s -w" -o dist/taskview.exe .
```

**POSIX (Bash / Zsh):**
```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/taskview .
```

### 交叉编译（Cross Compilation）示例

**编译 Linux (x86_64) 二进制：**
- PowerShell:
  ```powershell
  $env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
  go build -trimpath -ldflags="-s -w" -o dist/taskview-linux-amd64 .
  ```
- POSIX:
  ```bash
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/taskview-linux-amd64 .
  ```

**编译 macOS (Apple Silicon arm64) 二进制：**
- PowerShell:
  ```powershell
  $env:CGO_ENABLED="0"; $env:GOOS="darwin"; $env:GOARCH="arm64"
  go build -trimpath -ldflags="-s -w" -o dist/taskview-darwin-arm64 .
  ```
- POSIX:
  ```bash
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/taskview-darwin-arm64 .
  ```

### 配置系统 PATH（可选）

为方便在任意工作目录直接执行 `taskview`：

**Windows PowerShell:**
```powershell
# 临时添加到当前 PowerShell 窗口 PATH
$env:PATH += ";C:\path\to\TaskView\dist"

# 或持久化添加至当前用户 PATH
[Environment]::SetEnvironmentVariable(
    "Path",
    [Environment]::GetEnvironmentVariable("Path", "User") + ";C:\path\to\TaskView\dist",
    "User"
)
```

**POSIX (Bash / Zsh):**
```bash
export PATH="$PATH:/path/to/TaskView/dist"
```

---

## 🗄️ 系统架构与数据存储

```text
[ CLI 数据命令 (add/list/update) ] ──────────(直连本地 SQLite)────────┐
                                                                        ▼
                                                              ~/.taskview/tasks.db
                                                                        ▲
[ 本地浏览器 Web 看板 ] ──────(REST API: 127.0.0.1:<port>)─────┐        │
                                                               │        │
[ CLI 管理 (ui / server) ] ───(Token 认证: server.json)──► [ 后台守护进程 ] ────┘
                                                               ▲
[ Windows 登录事件 (可选) ] ────(Task Scheduler 登录自启)───────┘
```

1. **存储引擎**：
   - 纯 Go 嵌入式 SQLite 驱动（`modernc.org/sqlite`），开启 WAL 模式。
   - 数据存储路径默认为 `~/.taskview/tasks.db`，可由环境变量 `TASKVIEW_HOME` 自定义目录。
   - 所有 CLI 数据操作（`add`、`list` 等）均直接连接 SQLite，无需启动 Web 服务。
   - 严格沿用已有表结构（`tasks` 表与索引），以当前工作目录（或显式 `--project`）记录，兼容已有历史数据与任务 ID 生成规则。
2. **服务与接口**：
   - 本地回环接口服务（`127.0.0.1`，默认端口 3333，占用时自动顺延最多 10 个端口）。
   - 无多用户/团队云端同步，回环绑定不作为身份验证机制。
   - 实例生命周期管理通过 `~/.taskview/server.json` 中的私有 Token 鉴权通信，健康检查纯只读，绝不误杀无关进程。
   - 提供 `/api/tasks`, `/api/projects`, `/api/stats` 等 REST API。
3. **前端静态资源**：
   - 单文件网页通过 Go 1.16+ `go:embed` 嵌入到可执行程序中。
   - 样式与字体目前仍由浏览器向第三方 CDN（Tailwind CDN 与 Google Fonts）发起外部网络请求，未缓存时需要网络连接，并非完全离线网络隔离。离线静态资源有待后续版本实现本地化打包。
