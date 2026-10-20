# TaskView Agent Skill 配置与导入指南

本目录包含面向 AI 编程助手（Agent）的 **TaskView Agent Skill**。它遵循通用的 [Agent Skills 标准规范](https://agentskills.io/specification)，为支持 Skill 机制的 AI 宿主环境（如 OpenCode、Claude Code 等）提供调用 TaskView CLI 记录、跟踪和流转代码任务的标准指令。

---

## 🌟 核心特性：随 Skill 附带程序（Self-Contained Bundle）

本技能目录采用**自包含多平台模式**：
- **目录即完整技能包**：`skills/task-view` 目录内置了支持 Windows、Linux 及 macOS 的静态编译单二进制程序（位于 `scripts/` 子目录下）。宿主工具可直接配置使用本目录或整体复制本目录，无需外部编译或单独安装。
- **免安装运行时与环境变量**：终端用户使用本 Skill **无需安装 Node.js、Go 运行时环境，无需系统预装 SQLite，亦无需向系统 PATH 添加环境变量**。Agent 会根据当前操作系统与 CPU 架构自动以绝对路径调用匹配的包内程序。
- **开箱即用与版本可控**：内置程序均经过纯 Go 静态编译（`CGO_ENABLED=0`），随技能包一同版本化维护。

---

## 📦 技能包内容结构

`skills/task-view` 完整目录结构包含以下 5 个文件：

```text
task-view/
├── SKILL.md                 # Agent Skill 核心规范与指令定义（含标准 YAML Frontmatter）
├── README.md                # 用户配置与宿主导入指引（即本文档）
└── scripts/
    ├── taskview.exe          # Windows (x86_64 / amd64) 独立可执行程序
    ├── taskview-linux-amd64  # Linux (x86_64 / amd64) 独立可执行程序
    └── taskview-darwin-arm64 # macOS (Apple Silicon / arm64) 独立可执行程序
```

---

## 🚀 导入宿主环境（Import Instructions）

### 导入方式

直接将整个 `task-view` 文件夹复制到宿主工具支持的技能目录下，或在宿主工具中直接配置本目录路径。  
*注意：必须使用或复制整个 `task-view` 文件夹（包含 `SKILL.md`、`README.md` 与 `scripts/` 子目录中的 3 个平台程序），切勿仅复制单独的 `SKILL.md`，亦切勿误将上层的 `skills` 目录当作技能包导入。*

确保最终目标路径形态为 `<skills-dir>/task-view/SKILL.md`。

常见宿主的目标路径示例：

### 1. OpenCode (V2)
- **用户全局级别**：`~/.config/opencode/skills/task-view/SKILL.md`  
  *(若系统设置了 `XDG_CONFIG_HOME` 环境变量，则对应为 `$XDG_CONFIG_HOME/opencode/skills/task-view/SKILL.md`)*
- **项目工程级别**：`<project-root>/.opencode/skills/task-view/SKILL.md`
- 官方文档：[OpenCode Skills 文档](https://opencode.ai/v2/docs/skills/)

### 2. Claude Code
- **用户全局级别**：`~/.claude/skills/task-view/SKILL.md`
- **项目工程级别**：`<project-root>/.claude/skills/task-view/SKILL.md`
- 官方文档：[Claude Code Skills 文档](https://code.claude.com/docs/en/skills)

### 3. 其他支持 Agent Skills 规范的宿主
- 查阅对应宿主工具的技能文档，将 `task-view` 文件夹复制至其识别的技能目录。
- 协议规范：[Agent Skills Specification](https://agentskills.io/specification)

### 生效方式
完成文件放置后，**重启宿主或按宿主文档重新加载**即可生效（无需执行任何自定义导入命令）。

---

## ⚠️ 权限与运维注意事项

1. **本地进程执行权限与安全提示**：
   - 技能包中包含实际的可执行程序。AI 宿主客户端必须允许执行本地外部进程。
   - **Linux / macOS**：复制后若未保留执行权限，请对包内选定的程序执行 `chmod +x`。例如：`chmod +x '/实际Skill目录/task-view/scripts/taskview-linux-amd64'`（替换为实际绝对路径）。
   - **安全提示**：首次运行时若触发 Windows SmartScreen 或 macOS 安全与隐私拦截（Gatekeeper / Quarantine），请按操作系统标准提示允许运行。
2. **服务运行中的文件锁定保护（Pin Skill Folder）**：
   - 若通过 `taskview ui` 或 `taskview server start` 启动了后台 Web 看板服务，后台进程将持续占用对应可执行文件。
   - **升级、移动或删除 Skill 目录前，先停止由该程序运行的后台服务**。使用包内对应程序的绝对路径调用 `server stop`（例如 PowerShell：`& 'C:\实际Skill目录\task-view\scripts\taskview.exe' server stop`；POSIX：`'/实际Skill目录/task-view/scripts/taskview-linux-amd64' server stop`）。
3. **数据独立性与持久化**：
   - 任务数据库默认存储在用户全局目录 `~/.taskview/tasks.db`，不在 Skill 目录内。若设置了 `TASKVIEW_HOME`，请在升级时保留该目录并使用相同设置，不要将它放进可能被替换或删除的 Skill 目录。
4. **网络与安全声明**：
   - CLI 命令直连本地 SQLite，无外部网络请求。
   - Web 看板仅在用户显式要求时启动，监听本地回环地址（`127.0.0.1`）；前端页面在首次未缓存加载时会向第三方 CDN（Tailwind CDN、Google Fonts）发起网络请求，非绝对网络隔离环境。
   - 严禁在任务记录中包含密码、Token、API Key 等敏感凭据。

---

## 🛠️ 维护者构建命令

若从源码修改了 Go 代码或更新了文档，维护者可执行以下脚本同步刷新 `skills/task-view/scripts/` 内的 3 个平台可执行程序（需要 Go 1.26.5+ 编译环境）：

```powershell
pwsh -File ./scripts/build-skills.ps1
```
