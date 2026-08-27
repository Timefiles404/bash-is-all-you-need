---
name: wire-probe
description: 用 curl 探测 LLM 网关并把它实际发送的东西记录在 docs/wire-notes.md
---

# 探测网关

这个仓库里的每个教学声明都建立在 `docs/wire-notes.md` 上，它记录一个真实网关实际发出的东西 —— 逐字节，带上原始证据。协议文档和观测到的行为不一致，在它们不一致的地方观测赢了。

## 设置

凭证活在仓库根的 gitignored `.env` 里。这个宿主里的 shell 在调用间不持久化，所以在每个命令里源它：

```sh
set -a && . ./.env && set +a
```

在同一个密钥上两个端点：`$AGENT_BASE_URL/chat/completions`（OpenAI 协议）和 `$AGENT_BASE_URL/messages`（Anthropic 协议）。

## 方法

- 用 `curl`，不是 Agent。重点是看线上，不是一个对它的渲染。
- 把响应首先捕捉到一个文件，然后检查它。一个在进行中 grep 的管道丢弃了你十分钟后会想要的证据。
- 对流式，用 `--no-buffer` 捕捉并保留每个 SSE 帧，包括看起来像噪声的那些。`ping` 帧在 `message_start` 之前和 `message_stop` 之后是一个真实发现。
- 故意探测失败路径：一个坏的模型 id、一个坏的密钥、一个格式错误的请求体、一个丢失的必需字段、一个足够低的 `max_tokens` 来截断一个工具调用中间的参数。失败形状是两个协议最不一样的地方，也是文档最不可靠的地方。

## 记录

给 `docs/wire-notes.md` 增加一个编号的部分，有：

- 确切的 `curl` 调用，密钥打了码。
- 原始的响应体或 SSE 帧，逐字，只在注明的地方省略。
- 一句话讲发现是什么。
- 一个注记，如果它与协议文档矛盾，要讲文档说什么。

永远不要凭着对某个规范的记忆写声明。如果它不在 wire-notes 里带上证据，它不去一个章节。
