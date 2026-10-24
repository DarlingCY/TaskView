# TaskView Agent Skill 配置与导入指南

本目录是面向 AI 编程助手（Agent）的 **TaskView Agent Skill**，遵循通用的 [Agent Skills 标准规范](https://agentskills.io/specification)，为支持 Skill 机制的宿主环境（如 OpenCode、Claude Code 等）提供“用 TaskView 记录、跟踪、流转代码任务”的标准指令。

---

## 🌟 核心特性：原生 CLI 直连本地数据库

- **原生 CLI 操作**：Agent 统一通过控制台工具 `taskview-cli`（Windows 下为 `taskview-cli.exe`）读写任务数据。直接操作 SQLite 数据库，**不依赖 Web 服务是否在线，不发起任何 HTTP 请求**。
- **随包即用**：`scripts/<os>-<arch>/` 自带六个平台的静态 CLI（`CGO_ENABLED=0`），无需安装 Go、Node.js、外部 SQLite 驱动，也不需要配置 PATH。
- **跨平台支持**：提供 Windows、Linux、macOS（amd64 / arm64）预编译控制台二进制。
- **数据统一**：与 TaskView 桌面看版共用数据库（默认路径为 `~/.taskview/tasks.db` 或 `%USERPROFILE%\.taskview\tasks.db`，亦支持环境变量 `TASKVIEW_HOME`）。
- **看板边界**：可视化看板服务通过 Windows 桌面托盘应用提供（`127.0.0.1:3333`）。Linux/macOS 当前仅支持原生 CLI 工具，未提供桌面 GUI/托盘。

---

## 📦 技能包内容结构

```text
task-view/
├── SKILL.md    # Agent Skill 核心规范与指令定义
├── README.md   # 用户配置与宿主导入指引
└── scripts/
    ├── windows-amd64/taskview-cli.exe
    ├── windows-arm64/taskview-cli.exe
    ├── linux-amd64/taskview-cli
    ├── linux-arm64/taskview-cli
    ├── darwin-amd64/taskview-cli
    └── darwin-arm64/taskview-cli
```

---

## 🚀 导入宿主环境（Import Instructions）

从 [Releases](https://github.com/DarlingCY/TaskView/releases) 下载 `taskview-skill-<version>.zip` 并核对 `SHA256SUMS`，解压后将整个 `task-view` 文件夹复制到宿主工具支持的技能目录下；也可直接使用仓库中的本目录。

*必须复制整个 `task-view` 文件夹（包括 `SKILL.md`、`README.md` 和 `scripts/`），不能仅复制 `SKILL.md`，也不要误将上层的 `skills` 目录当作技能包。Linux/macOS 从 ZIP 导入后若提示无执行权限，对所选 CLI 执行 `chmod +x`。*

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

查阅对应宿主工具的技能文档，将 `task-view` 文件夹复制至其识别的技能目录。

---

## 🔧 随包 CLI 定位与验证

按**执行环境**而非宿主桌面系统选择 `scripts/<os>-<arch>/`，路径相对于技能根目录。Agent 使用绝对路径调用；项目工作目录不必是技能目录。

- Windows：x64 选 `windows-amd64/taskview-cli.exe`，ARM64 选 `windows-arm64/taskview-cli.exe`。
- Linux（含 WSL）：`uname -m` 为 `x86_64` 选 `linux-amd64/taskview-cli`；`aarch64` / `arm64` 选 `linux-arm64/taskview-cli`。
- macOS：Intel 选 `darwin-amd64/taskview-cli`，Apple Silicon 选 `darwin-arm64/taskview-cli`。

```sh
CLI='/absolute/path/task-view/scripts/linux-amd64/taskview-cli'
chmod +x "$CLI"
"$CLI" --version
```

```powershell
$Cli = 'C:\path\to\task-view\scripts\windows-amd64\taskview-cli.exe'
& $Cli --version
```

`--version` / `--help` 不访问数据库；`health` 会打开或初始化数据库，不是无副作用的安装检查。Windows 与 WSL 默认 HOME 不同；若要与 Windows 看板共享数据库，需要用户明确设置 WSL 的 `TASKVIEW_HOME` 为对应 `/mnt/c/.../.taskview` 目录。

从源码刷新全部六份随包 CLI 与发布压缩包：

```sh
bash scripts/build-cli.sh 2.1.0
```

或在仓库根使用 PowerShell：`pwsh -File ./scripts/build-cli.ps1 -Version 2.1.0`。构建会同步更新本目录 `scripts/`，并在 `dist/` 生成独立 CLI 压缩包、完整 skill ZIP 和 `SHA256SUMS`。源代码变化后、提交或发布前应重建随包 CLI。

### 备用：独立 CLI 安装与定位

以下仅在随包 CLI 缺失、或需要在终端独立使用时参考；导入完整技能包无需安装或修改 PATH。

### Windows
- 若通过安装包安装了 TaskView，控制台可执行文件位于：
  `%LOCALAPPDATA%\Programs\TaskView\taskview-cli.exe`
- 建议将该目录添加至用户的 `Path` 环境变量，或在 Agent 执行环境中直接以绝对路径调用。
- PowerShell 5.1 与 7+ 均可使用常规 flags 传入中文和带空格的路径（均已实测）；内联 `--json` 在 PowerShell 5.1 中会丢失双引号，请改用 PowerShell 7+（或用常规 flags）。

### Linux & macOS
- 从 Releases 下载对应系统的压缩包：
  - Linux: `taskview-cli-<version>-linux-amd64.tar.gz` 或 `taskview-cli-<version>-linux-arm64.tar.gz`
  - macOS: `taskview-cli-<version>-darwin-amd64.tar.gz` 或 `taskview-cli-<version>-darwin-arm64.tar.gz`
- 压缩包内的成员名固定为 `taskview-cli`（不带版本号）。解压后将其放置于系统的 PATH 路径中（例如 `/usr/local/bin/taskview-cli` 或 `~/.local/bin/taskview-cli`），并赋予执行权限：
  ```sh
  chmod +x /usr/local/bin/taskview-cli
  ```
- 验证安装：
  ```sh
  taskview-cli --version
  taskview-cli health
  ```
