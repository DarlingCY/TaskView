# TaskView

面向开发者与 AI Agent 协作场景的**本地任务与优化项跟踪系统**。

TaskView 是一个纯 Go 编写的 Windows 桌面应用：单文件可执行程序 `TaskView.exe`，内嵌 Web 看板与本地 HTTP API，数据保存在本机 SQLite 中。所有读写都通过 `127.0.0.1` 上的回环接口完成，数据不出本机。

- **单二进制**：`CGO_ENABLED=0` 静态编译，运行不需要 Go / Node.js / SQLite 运行时，也不需要系统预装任何数据库驱动。
- **按用户安装**：不需要管理员权限，不注册系统服务，不修改 `PATH`，不写注册表启动项以外的机器级配置。
- **数据集中存储**：任务保存在 `%USERPROFILE%\.taskview\tasks.db`，升级与卸载都不会删除它。
- **Agent 友好**：全部操作都有 HTTP 接口，附带开箱即用的 [Agent Skill](skills/task-view)。

---

## 安装（Windows）

从 [Releases](https://github.com/DarlingCY/TaskView/releases) 下载 `TaskView-Setup-<版本>-windows-amd64.exe`，按提示完成安装。

- **按用户安装**：安装至 `%LOCALAPPDATA%\Programs\TaskView`，无需管理员权限。
- **快捷方式与自启**：默认创建开始菜单项；桌面快捷方式与登录自启可选（升级时保留既有自启设置）。
- **平滑升级与回滚**：升级时先停服替换，失败自动回滚；卸载时不删除 `%USERPROFILE%\.taskview\tasks.db`。
- **校验**：每个 Release 附带 `SHA256SUMS`；未做代码签名，SmartScreen 提示未知发布者属正常现象，核对摘要后放行即可。

---

## 运行时

TaskView 常驻系统托盘，并在 `127.0.0.1:3333` 提供 Web 看板与 HTTP API。

```powershell
$health = Invoke-RestMethod 'http://127.0.0.1:3333/api/health'
$health.success   # $true
$health.version   # 运行中的版本
```

健康检查失败说明服务没有运行：**启动已安装的应用**（托盘图标或开始菜单 `TaskView`），待其就绪后重试。

> `3333` 被占用时应用会自动顺延到下一个可用端口；此时以托盘「打开看板」实际打开的地址为准。

---

## HTTP API

所有接口都在 `http://127.0.0.1:3333` 之下，只接受回环地址的请求。Agent 接口统一位于 `/api/agent/*`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/agent/health` | 健康检查。`200` 返回 `{success,version}`；服务未就绪时返回 `503` |
| `GET` | `/api/agent/tasks` | 任务列表。需要 `project_path`（绝对路径） |
| `POST` | `/api/agent/tasks` | 新建任务，返回 `201` 与任务对象 |
| `GET` | `/api/agent/tasks/{id}` | 任务详情 |
| `PATCH` | `/api/agent/tasks/{id}` | 更新任务（只接受可写字段） |
| `POST` | `/api/agent/tasks/{id}/notes` | **原子追加**一条备注，body 为 `{"note":"…"}`，返回更新后的任务 |

### 任务字段

`id`、`project_path`、`project_name`、`title`、`status`、`priority`、`description`、`file_path`、`code_snippet`、`agent_notes`、`line_start`、`line_end`、`created_at`、`updated_at`、`closed_at`。

- `status`：`backlog` \| `todo` \| `in_progress` \| `review` \| `done`（默认 `todo`）
  - `review`（待审查验证）：实现已完成、等待人工或 Agent 复核的阶段。仍算**未完成**，不设置 `closed_at`。
  - 状态之间可任意流转，不强制审批顺序；只有 `done` 会写入 `closed_at`。
- `priority`：`low` \| `medium` \| `high` \| `urgent`（默认 `medium`）
- `project_path` **必须是绝对路径**，相对路径直接 `400`。
- `description` / `file_path` / `code_snippet` / `agent_notes` 为可空字段。

### 查询参数

| 参数 | 说明 |
| --- | --- |
| `project_path` | 工程绝对路径，必填 |
| `status` / `priority` | 单值过滤（可选），不支持逗号分隔或重复传入同名参数 |

```powershell
$base = 'http://127.0.0.1:3333'
$project = 'D:\Work\MyApp'

Invoke-RestMethod "$base/api/agent/tasks?project_path=$([uri]::EscapeDataString($project))"
Invoke-RestMethod "$base/api/agent/tasks?project_path=$([uri]::EscapeDataString($project))&status=todo&priority=high"
```

### 新建任务

```powershell
$body = @{
    project_path = 'D:\Work\MyApp'
    title        = '重构 Auth 中间件的 Token 缓存'
    priority     = 'high'
    description  = '高并发下每次请求都会回源数据库'
    file_path    = 'src\middleware\auth.go'
    line_start   = 42
    line_end     = 65
} | ConvertTo-Json

Invoke-RestMethod -Method Post -Uri "$base/api/agent/tasks" -ContentType 'application/json' -Body $body
```

### 更新与追加备注

```powershell
Invoke-RestMethod -Method Patch -Uri "$base/api/agent/tasks/T202610190001" `
    -ContentType 'application/json' -Body (@{ status = 'in_progress' } | ConvertTo-Json)

# 实现完成，等待复核：置为 review（仍算未完成）
Invoke-RestMethod -Method Patch -Uri "$base/api/agent/tasks/T202610190001" `
    -ContentType 'application/json' -Body (@{ status = 'review' } | ConvertTo-Json)

# 验证通过后才关闭
Invoke-RestMethod -Method Patch -Uri "$base/api/agent/tasks/T202610190001" `
    -ContentType 'application/json' -Body (@{ status = 'done' } | ConvertTo-Json)

# 原子追加，不覆盖既有备注
Invoke-RestMethod -Method Post -Uri "$base/api/agent/tasks/T202610190001/notes" `
    -ContentType 'application/json' -Body (@{ note = '基准测试完成：延迟下降 35%' } | ConvertTo-Json)
```

---

## Agent Skill

[`skills/task-view`](skills/task-view) 是标准 Agent Skill（[Agent Skills 规范](https://agentskills.io/specification)），只通过上面的 HTTP API 工作。导入方式见 [skills/task-view/README.md](skills/task-view/README.md)。

---

## 从源码构建

仅支持 Windows 构建。需要 Windows 10 或更高版本，以及 Go 1.26.5+ 与 Inno Setup 6。

```powershell
pwsh -File ./scripts/build-windows.ps1            # 输出安装包至 dist\
pwsh -File ./scripts/build-windows.ps1 -Version 2.1.0
```

脚本会自动嵌入图标与应用清单、完成 GUI 静态编译、调用 Inno Setup 生成安装包并计算 `SHA256SUMS`。

---

## 安全说明

- 服务只监听 `127.0.0.1`，但回环绑定不等于身份认证，本机其他进程亦可访问。
- 前端页面初次加载会向 CDN（Tailwind CDN、Google Fonts）请求样式资源，非绝对离线环境。
- 请勿在任务内容中记录密码、Token 等敏感凭据。
