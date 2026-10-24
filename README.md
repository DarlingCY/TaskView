# TaskView

面向开发者与 AI Agent 协作场景的**本地任务与优化项跟踪系统**。

TaskView 是一个纯 Go 编写的本地任务管理系统：
- **Web 看板（Windows）**：单文件桌面托盘应用 `TaskView.exe`，在 `127.0.0.1:3333` 提供 Web 看板界面与 Board API，数据保存在本机 SQLite 中。
- **原生 CLI（跨平台）**：控制台工具 `taskview-cli`（Windows 下为 `taskview-cli.exe`），支持 **Windows、Linux、macOS**（amd64 / arm64）。Agent 通过原生 CLI 直接读写任务数据库，不依赖 Web 看板是否运行，无需发起任何 HTTP 请求。

- **单二进制**：`CGO_ENABLED=0` 静态编译，运行不需要 Go / Node.js / 外部 SQLite 驱动，数据不出本机。
- **按用户安装**：Windows 安装器无需管理员权限，安装包自带桌面应用与控制台 CLI，不注册系统服务，不擅自修改系统 PATH。
- **数据集中存储**：任务保存在 `~/.taskview/tasks.db`（Windows 为 `%USERPROFILE%\.taskview\tasks.db`），支持环境变量 `TASKVIEW_HOME` 自定义目录。
- **Agent 友好**：全部任务操作均由原生 CLI 支持标准 flags 与内联 `--json`，附带开箱即用的 [Agent Skill](skills/task-view)。

---

## 安装

### Windows
从 [Releases](https://github.com/DarlingCY/TaskView/releases) 下载 `TaskView-Setup-<版本>-windows-amd64.exe`，按提示完成安装。
- **桌面与 CLI 一体**：安装至 `%LOCALAPPDATA%\Programs\TaskView`，内含 `TaskView.exe` 与 `taskview-cli.exe`。
- **快捷方式与自启**：默认创建开始菜单项；桌面快捷方式与登录自启可选（升级时保留既有自启设置）。
- **平滑升级与回滚**：升级时先停服替换，失败自动回滚；卸载时不删除 `%USERPROFILE%\.taskview\tasks.db`。
- **校验**：每个 Release 附带 `SHA256SUMS`；未做代码签名，核对摘要后放行即可。

> **Windows PowerShell**：5.1 与 7+ 都可使用常规 flags（中文与空格路径已实测）。PowerShell 5.1 向原生 exe 传递内联 JSON 时会丢失双引号，因此 `--json`（包括用 `null` 清空字段）请使用 PowerShell 7+；5.1 请只使用 flags。

### Linux & macOS
从 [Releases](https://github.com/DarlingCY/TaskView/releases) 下载对应系统的预编译 CLI 压缩包：
- Linux: `taskview-cli-<版本>-linux-amd64.tar.gz` 或 `taskview-cli-<版本>-linux-arm64.tar.gz`
- macOS: `taskview-cli-<版本>-darwin-amd64.tar.gz` 或 `taskview-cli-<版本>-darwin-arm64.tar.gz`

压缩包内的成员名为 `taskview-cli`（不带版本号）。解压并添加可执行权限，放置于系统的 PATH 路径中（如 `/usr/local/bin`）：
```sh
tar -xzf taskview-cli-*.tar.gz
chmod +x taskview-cli
sudo mv taskview-cli /usr/local/bin/
```
> **注意**：Linux 与 macOS 当前仅提供原生控制台 CLI 支持，未提供桌面 GUI/托盘/安装器。

---

## 运行时架构与边界

| 组件 | 支持平台 | 运行方式 | 数据源 | 说明 |
| --- | --- | --- | --- | --- |
| **Web 看板** | Windows | 桌面托盘服务，监听 `127.0.0.1:3333` | `tasks.db` | 供人工在浏览器查看看板与流转任务，接口位于 `/api/board/*` |
| **taskview-cli** | Windows / Linux / macOS | 原生控制台命令（独立进程） | `tasks.db` | 供 AI Agent 或终端开发者直接操作，无 Web/HTTP 依赖 |

> `tasks.db` 支持多进程通过 SQLite WAL 模式安全并发读写。

---

## taskview-cli 命令指南

`taskview-cli` 默认输出稳定机器可读的 JSON 到 **stdout**（退出码 `0`）；失败时错误 JSON 输出到 **stderr**（退出码非零）。`--help` 与 `--version` 不会触碰数据库。

### 退出码与错误格式

- `0`：成功
- `1`：参数错误 / 校验失败（`validation_error`）
- `2`：任务不存在（`not_found`）
- `3`：数据库或系统内部错误（`internal_error`）

错误响应格式（stderr）：
```json
{
  "success": false,
  "error": "project_path is required and must be an absolute path",
  "code": "validation_error"
}
```

### 命令列表

#### 1. 健康检查
```sh
taskview-cli health
```
输出：`{"success":true,"version":"2.1.0"}`

#### 2. 任务列表
`--project-path` 为必填项，必须是绝对路径。
```sh
taskview-cli tasks list --project-path "/path/to/project"
# 可选过滤 status 与 priority：
taskview-cli tasks list --project-path "/path/to/project" --status todo --priority high
```

#### 3. 获取任务详情
```sh
taskview-cli tasks get T202610190001
# 只读核对工程路径（不会修改任务）：
taskview-cli tasks get T202610190001 --project-path "/path/to/project"
```

#### 4. 新建任务
支持标准 flags 传参或通过 `--json` 传入完整对象：
```sh
# 方式 A：标准 flags
taskview-cli tasks create   --project-path "/path/to/project"   --title "重构 Auth 中间件的 Token 缓存"   --priority high   --status todo   --description "高并发下每次请求都会回源数据库校验 Token"   --file-path "src/middleware/auth.go"   --line-start 42   --line-end 65   --code-snippet "token := db.Query(ctx, sql)"   --agent-notes "建议引入本地 LRU 缓存"

# 方式 B：内联 --json
taskview-cli tasks create --json '{
  "project_path": "/path/to/project",
  "title": "重构 Auth 中间件的 Token 缓存",
  "priority": "high",
  "status": "todo",
  "description": "高并发下每次请求都会回源数据库校验 Token"
}'
```

#### 5. 更新任务
```sh
# 标准 flags
taskview-cli tasks update T202610190001 --status in_progress --priority urgent

# 归属保护：任务不属于该工程时拒绝更新，且不修改任务
taskview-cli tasks update T202610190001 --verify-project-path "/path/to/project" --status review

# 迁移任务到另一个工程（会改写 project_path，不是保护参数）
taskview-cli tasks update T202610190001 --project-path "/path/to/other-project"

# 内联 --json（支持将字段置为 null 以清空）
taskview-cli tasks update T202610190001 --json '{
  "status": "in_progress",
  "description": "更新后的任务描述",
  "code_snippet": null
}'
```

#### 6. 追加备注（原子操作）
在现有 `agent_notes` 末尾原子追加新内容，不覆盖历史备注：
```sh
taskview-cli tasks notes T202610190001 "基准测试完成：延迟下降 35%"
```

#### 7. 驳回任务（Reject）
代码审查或测试未通过时使用，任务状态回退一档并记录驳回理由至 `rejection_reason`：
```sh
taskview-cli tasks reject T202610190001 "缓存未失效，鉴权仍会回源数据库"
```

**状态回退规则**：
- `review` → `in_progress`
- `in_progress` → `backlog`
- `todo` → `backlog`
- `backlog` / `done` 无法驳回（返回退出码 `1`）

---

## 任务字段与生命周期

- `status`：`backlog` \| `todo` \| `in_progress` \| `review` \| `done`（默认 `todo`）
  - `review`（待审查验证）：实现已完成、等待人工或 Agent 复核的阶段。仍算**未完成**，`closed_at` 保持为 `null`。
  - 只有推进至 `done` 才会写入 `closed_at`；从 `done` 移出时清空 `closed_at`。
  - 推进至 `review` 或 `done` 时由底层数据库自动清空 `rejection_reason`。
- `priority`：`low` \| `medium` \| `high` \| `urgent`（默认 `medium`）
- `project_path`：**必须是绝对路径**。

---

## Agent Skill

[`skills/task-view`](skills/task-view) 是标准 Agent Skill（遵循 [Agent Skills 规范](https://agentskills.io/specification)），自带 Windows / Linux / macOS（amd64 / arm64）原生 CLI，位于 `scripts/<os>-<arch>/`，无需安装运行时、配置 PATH 或启动看板。

从 Releases 下载 `taskview-skill-<版本>.zip`，核对 `SHA256SUMS` 后将完整 `task-view` 文件夹（包括 `scripts/`）导入宿主技能目录即可。Linux/macOS 从 ZIP 导入后可能需要对所选 CLI 执行 `chmod +x`。配置与导入指引见 [skills/task-view/README.md](skills/task-view/README.md)。

---

## 从源码构建

需要 Go 1.26+。

### 1. 构建 Windows 桌面安装器与 CLI
（需要 Windows 环境与 Inno Setup 6）
```powershell
pwsh -File ./scripts/build-windows.ps1 -Version 2.1.0
```

### 2. 构建三平台 CLI 二进制与摘要
支持在 Windows（PowerShell）或 Linux/macOS（Bash）下交叉编译：
```powershell
# PowerShell:
pwsh -File ./scripts/build-cli.ps1 -Version 2.1.0

# 或 Bash:
./scripts/build-cli.sh 2.1.0
```
产物与 `SHA256SUMS` 将输出至 `dist/` 目录，包括六个平台的 CLI 压缩包和自包含的 `taskview-skill-<版本>.zip`；构建同时刷新 `skills/task-view/scripts/<os>-<arch>/` 中的随包二进制。修改 CLI 源码后，提交或发布前应重建这些二进制。
