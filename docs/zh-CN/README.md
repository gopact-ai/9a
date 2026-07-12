# NineA

[English](../../README.md)

## 将任意能力变成每个 Agent 都能使用的 Skill

NineA 将 MCP 工具、A2A Agent 和 JSON HTTP API 转换为文件系统原生的
Skill，让不同 Agent 都能发现、检查和调用这些能力。

```text
MCP tools ─┐
A2A agents ├─→ adapters → Catalog → filesystem Skills → any agent
HTTP APIs ─┘                    │
                               └─→ authorized command execution
```

Agent 已经熟悉两种稳定接口：

- **文件系统负责发现和上下文**：说明、schema 和来源都是可检查的普通文件；
- **命令行负责执行**：显式命令把被动读取和经过授权的上游操作分开。

NineA 不会把完整能力目录塞进 prompt。Agent 先在本地搜索，再只把需要的能力
投影到自己的 Skill 目录，最后通过生成的脚本调用。Agent 不需要内置 MCP client、
A2A client、API 凭证或厂商专用工具注册表。

## 安装

Homebrew 会在 macOS 或 Linux 上同时安装 `9a` client 和 `ninead` daemon：

```sh
brew install gopact-ai/tap/ninea
```

[GitHub Releases](https://github.com/gopact-ai/9a/releases) 同时提供发布归档和
SHA-256 校验文件。NineA 当前提供 macOS 和 Linux 的 x86-64、ARM64 二进制文件。

## 从能力到 Skill

```sh
9a search "weather temperature"
9a project add mcp/weather/get-weather .agents/skills
printf '%s\n' '{"location":"Shanghai"}' | \
  .agents/skills/ninea-mcp-weather-get-weather/scripts/invoke
```

投影后的 Skill 包含 `SKILL.md`、`schema.json`、有界的上游来源信息和
`scripts/invoke`。搜索和读取文件都是本地操作，不会访问 provider；真正调用还需要
独立的 `invoke` 权限。

需要脱离单次 CLI 请求持续跟踪的工作，可以启动持久调用：

```sh
CALL_ID="$(printf '%s\n' '{"location":"Shanghai"}' | \
  9a calls start mcp/weather/get-weather)"
9a calls get "$CALL_ID"
9a calls events "$CALL_ID" --limit 100
```

调用状态、结果和可分页事件会保存到 SQLite。只有 capability 明确声明可取消，且
adapter 能确认取消时，`calls cancel` 才可用。

## 当前已经实现

| 集成 | 当前 Alpha 状态 |
| --- | --- |
| MCP 工具 | 内置本地 stdio adapter |
| A2A Agent | 内置 HTTP+JSON 1.0 adapter；支持单轮 Skill 和异步 Task |
| JSON HTTP API | manifest 驱动的[通用 HTTP adapter](../../examples/http-adapter/README.md) |
| 自定义协议 | 可持久注册语言无关的 `9a.adapter/v1` executable adapter |
| Agent 接口 | 本地搜索、选择性文件系统 Skill 投影、同步与持久异步执行 |
| 访问控制 | bearer 身份、默认拒绝的 capability ACL、独立 `read` 和 `invoke` 权限 |

Executable adapter 的 wire contract 和注册流程见英文
[Building adapters](../adapters.md)。

## 为什么采用这种形态

如果没有共同能力层，每个 Agent 都要分别集成每种上游协议，形成 N × M 问题。
NineA 让上游只需一个 adapter、Agent 只需一种 Skill 格式，并统一负责规范化、本地
搜索、授权、持久化和路由。

设计受到 Plan 9 namespace 模型启发：把异构资源通过小而一致的接口呈现，并组装到
调用者选择的 namespace 中，会更容易理解和组合。NineA 不实现 9P，也不把远程操作
伪装成文件；文件用于披露能力，命令用于执行操作。详见英文
[Architecture and Plan 9](../architecture.md)。

## 当前边界

NineA 目前是用于本地评估的 Alpha，需要 Unix domain socket。当前不提供 provider
sandbox、HTTP MCP transport、流式执行、多轮续接或稳定兼容承诺。MCP server 和
executable adapter 都是受信本地进程，以 daemon 用户的操作系统权限运行。

## 从这里开始

- [Getting started](../getting-started.md)：可复制的 MCP 流程、认证、集成和完整 CLI
- [Building adapters](../adapters.md)：executable protocol 和 registry
- [Generic HTTP adapter](../../examples/http-adapter/README.md)：通过 manifest 连接
  JSON API
- [Architecture and Plan 9](../architecture.md)
- [Security](../SECURITY.md)

运行完整进程级集成测试：

```sh
go test -count=1 ./test/e2e
```

NineA 使用 [MIT License](../../LICENSE)。
