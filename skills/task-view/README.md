# TaskView Agent Skill 配置与导入指南

本目录是面向 AI 编程助手（Agent）的 **TaskView Agent Skill**，遵循通用的 [Agent Skills 标准规范](https://agentskills.io/specification)，为支持 Skill 机制的宿主环境（如 OpenCode、Claude Code 等）提供"用 TaskView 记录、跟踪、流转代码任务"的标准指令。

---

## 🌟 核心特性：本地 HTTP API

- **技能包只含指令**：目录内只有 `SKILL.md` 与本文件。Agent 通过 TaskView 的**本地 HTTP API**（`127.0.0.1`）读写任务。
- **免安装运行时**：终端用户使用本 Skill **无需安装 Go / Node.js / SQLite 运行时**。
- **前置条件只有一个**：Windows 上已安装并启动 TaskView 桌面应用（见仓库根目录 README 的安装章节）。接口默认地址为 `http://127.0.0.1:3333`。

---

## 📦 技能包内容结构

```text
task-view/
├── SKILL.md    # Agent Skill 核心规范与指令定义（含标准 YAML Frontmatter）
└── README.md   # 用户配置与宿主导入指引（即本文档）
```

---

## 🚀 导入宿主环境（Import Instructions）

将整个 `task-view` 文件夹复制到宿主工具支持的技能目录下，或在宿主工具中直接配置本目录路径。

*注意：必须使用或复制整个 `task-view` 文件夹（包含 `SKILL.md` 与 `README.md`），切勿仅复制单独的 `SKILL.md`，亦切勿误将上层的 `skills` 目录当作技能包导入。*

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
协议规范：[Agent Skills Specification](https://agentskills.io/specification)

### 生效方式

完成文件放置后，**重启宿主或按宿主文档重新加载**即可生效（无需执行任何自定义导入命令）。

---

## ⚠️ 权限与运维注意事项

1. **前置服务**：TaskView 未运行时健康检查会失败，此时提示用户启动已安装的应用（托盘图标 / 开始菜单 `TaskView`），待其就绪后重试。
2. **数据独立性与持久性**：任务数据库位于用户全局目录 `%USERPROFILE%\.taskview\tasks.db`，**不在技能目录内**，升级或删除技能目录不会影响它。若设置了 `TASKVIEW_HOME`，请保持一致，不要把它放进会被替换的目录。
3. **升级 TaskView 本身**：升级安装会先停止正在运行的 TaskView 再替换程序；新版本起不来时安装程序会回滚到旧版本。任务数据与登录自启设置在升级中保持不变。
4. **网络与安全声明**：Agent 只访问本机回环地址（`127.0.0.1`），不向外部发送任何数据；回环绑定不等于身份认证，本机其他进程同样可访问该接口。前端页面在首次未缓存加载时会向第三方 CDN（Tailwind CDN、Google Fonts）发起网络请求。
5. **敏感信息**：严禁在任务标题、描述、备注中记录密码、Token、API Key 等凭据。

---

## 🛠️ 维护者说明

技能包内容全部为文本指令，随仓库一起维护，**不需要**任何构建脚本。修改 `skills/task-view/SKILL.md` 后即可生效。

与技能相关的接口变更，请同步更新：

- `skills/task-view/SKILL.md`（Agent 调用说明）
- 仓库根目录 `README.md`（面向用户的 API 文档）
