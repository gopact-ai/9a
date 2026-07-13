# NineA

[English](../../README.md)

## 🚀 把 API、MCP 工具和 A2A Agent 变成所有 Agent 都能使用的 Skill

NineA 是面向 AI Agent 的能力层。它把异构的上游系统转换为文件系统中可检查、可执行
的 Skills——这正是 Coding Agent 最擅长使用的交互入口。

```text
YAML APIs ─┐
MCP tools  ├──→ NineA Catalog ──→ 文件系统 Skills ──→ 任意 Agent
A2A agents ┘          │                    │
                      └── 本地搜索          └── 显式命令调用
```

接入一个 JSON API，只需要一个 YAML 文件：

```yaml
apiVersion: 9a.dev/v1alpha1
kind: Skill
metadata:
  name: weather
  description: 查询城市当前天气。
services:
  forecast:
    baseURL: https://api.open-meteo.com
operations:
  current-weather:
    service: forecast
    method: GET
    path: /v1/forecast
    request:
      query:
        latitude: "{{ input.latitude }}"
        longitude: "{{ input.longitude }}"
        current: temperature_2m
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body.current
```

```sh
9a validate weather.yaml
9a add weather.yaml
printf '%s\n' '{"latitude":31.2,"longitude":121.5}' | \
  .agents/skills/weather/operations/current-weather/invoke
```

结果是一个真实可见、由 9A 管理的只读 Skill，而不是隐藏在某个产品里的工具注册：

```text
.agents/skills/weather/
├── SKILL.md
├── operations/current-weather/
│   ├── schema.json
│   └── invoke
└── references/source.yaml
```

同一个 YAML 可以描述多个 API 地址、多个 operation 和顺序 workflow，并聚合为一个
业务域 Skill。变量由 daemon 的环境变量控制；请求前和响应后 hook 可以设置或删除
header，也可以用内置 jq 调整输入输出。签名等声明式能力无法覆盖的逻辑，可以显式
启用受超时、环境白名单和输出大小约束的 executable hook。

可以直接查看无需 API Key 的
[Open-Meteo 示例](../../examples/declarative/open-meteo.yaml)、把三个 API 聚合成一个
Skill 的 [API bundle 示例](../../examples/declarative/api-bundle.yaml)，以及完整的英文
[Declarative Skills 手册](../declarative-skills.md)。

Agent 第一次在 workspace 中执行搜索、添加或接入命令时，9A 还会自动挂载内置的
`using-ninea` Skill。Agent 可以直接从中学习如何发现、调用、添加、更新和排查能力，
用户不需要记忆 CLI 和 YAML schema。

## 🧭 为什么是文件系统和命令行

AI Agent 已经非常擅长两种稳定接口：

- **文件系统负责发现和上下文**：普通文件里的说明、schema、来源、本地搜索和按需
  加载；
- **命令行负责执行**：显式命令清楚地区分“读取一个能力”和“触发上游副作用”。

NineA 不会把全部能力塞进 prompt。Agent 先搜索本地 Catalog，只把当前需要的 Skill
加载到自己的 namespace，再用 JSON 调用一个小命令。消费方不需要内置 MCP client、
A2A client、API SDK、凭证处理或厂商专用工具注册表。

这一设计受到 Plan 9 namespace 的启发：adapter 用一组小而统一的接口隐藏异构协议，
调用方再组装自己所需的视图。NineA 不实现 9P，也不假装远程操作就是文件；文件披露
能力，命令执行操作。详见英文 [Architecture and Plan 9](../architecture.md)。

## 📦 安装和启动

Homebrew 会在 macOS 或 Linux 上安装唯一的 `9a` 命令：

```sh
brew install gopact-ai/tap/ninea
```

升级 `9a`：

```sh
brew upgrade gopact-ai/tap/ninea
```

可用以下命令确认 Homebrew 放到 `PATH` 中的版本，并在不启动 daemon 的
情况下查看完整参数：

```sh
9a version
9a --help
9a help calls events
```

升级后，使用 Homebrew service 的用户执行 `brew services restart ninea`，再执行
`9a update --check` 预览变化，执行 `9a update` 刷新内置 Skill 和当前 workspace 的
托管视图。软件升级与 workspace 更新的完整区别见英文
[Upgrade NineA](../getting-started.md#upgrade-ninea)。

[GitHub Releases](https://github.com/gopact-ai/9a/releases) 提供 macOS 和 Linux 的
x86-64、ARM64 归档及 SHA-256 校验文件。

在 workspace 中只需执行：

```sh
9a attach
```

`9a` 会按需启动本地 daemon，首次启动时自动创建私有 state 目录和管理员 token。
socket 与 token 默认从 `$HOME/.local/state/ninea` 读取，无需修改 shell 配置；
`NINEA_SOCKET` 和 `NINEA_TOKEN` 仍可显式覆盖。英文
[User Guide](../getting-started.md) 介绍持久化启动、版本升级、独立 Agent 身份、ACL、
MCP、A2A 和完整命令参考。

```sh
9a search "weather"
9a status --json
```

9A 会优先为每个托管 Skill 使用独立的 FUSE 只读挂载；系统不支持时，自动回退到带
完整性校验的只读真实文件，并在 `status` 中说明原因。它不会覆盖用户自己的 Skills。
`9a update` 用于重新发现 Provider、升级和修复视图，`9a detach` 只移除当前 workspace
的托管视图。

## 🔌 三种接入路径

| 上游 | 接入方式 | 适用场景 |
| --- | --- | --- |
| JSON HTTP API | 内置声明式 YAML | 单 API、业务域 API 聚合、环境变量、hook 和 workflow |
| MCP | 内置本地 stdio adapter | 已有 MCP server 和工具发现 |
| A2A | 内置 HTTP+JSON 1.0 adapter | 已有 Agent、Skill、异步 Task 和取消 |
| 其他协议 | 语言无关的 `9a.adapter/v1` executable | 自定义发现、流式语义、重试或非 HTTP transport |

MCP、A2A 和自定义 adapter 的能力会进入同一个 Catalog，并可按需投影：

```sh
9a search "weather temperature"
9a project add mcp/weather/get-weather .agents/skills
```

使用 `9a providers remove <protocol> <name>` 可以删除 Provider 及其产生的全部托管视图。

## 🔒 安全边界

NineA 使用 bearer 身份、私有 Unix socket 和默认拒绝的 capability ACL；读取与执行是
独立权限。远程 API 必须使用 HTTPS，本地 loopback 开发地址除外。YAML 会先经过严格
校验，凭证只保留环境变量引用。

Executable hook、MCP server 和自定义 executable adapter 都是可信本地代码，以
daemon 用户权限运行。需要更强隔离时，应使用独立操作系统账号或 sandbox。详见英文
[Security](../SECURITY.md)。

FUSE 提供内核级只读保证。目录回退能阻止普通工具误修改，并用 SHA-256 manifest
发现和修复变化，但无法阻止同一个操作系统账号主动修改自己的权限；需要严格不可变
时应使用 `9a attach --backend fuse`。

## 📚 文档

- [Declarative Skills](../declarative-skills.md)：YAML schema、变量、模板、hook、
  workflow、生命周期和故障排查
- [声明式示例](../../examples/declarative/README.md)：天气、认证 API、多 API 聚合和
  executable hook
- [User Guide](../getting-started.md)：Agent 使用方式、安装升级、daemon、身份与命令参考
- [Building adapters](../adapters.md)
- [Architecture and Plan 9](../architecture.md)
- [Security](../SECURITY.md)

运行包括进程级 E2E 在内的完整测试：

```sh
go test -count=1 ./...
```

NineA 使用 [MIT License](../../LICENSE)。
