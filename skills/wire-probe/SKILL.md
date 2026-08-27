---
name: wire-probe
description: 要看网关线上到底发什么，用 curl 探一次，记进 docs/wire-notes.md
---

# 探网关

这个仓库里每一条教学论断都由 `docs/wire-notes.md` 撑着：那份文件逐字节
记下了一个真实网关到底发出了什么，原始证据一并附上。协议文档和实测行为
对不上，对不上的地方实测赢。

## 准备

凭证在仓库根目录那个被 gitignore 的 `.env` 里。这个宿主的 shell 在两次
调用之间不留状态，所以每条命令里都得重新 source 一遍：

```sh
set -a && . ./.env && set +a
```

同一个 key 上挂着两个端点：`$AGENT_BASE_URL/chat/completions`（OpenAI
协议）和 `$AGENT_BASE_URL/messages`（Anthropic 协议）。

## 打法

- 用 `curl`，不要用 Agent。要看的是线上原样，不是它被渲染过一遍的样子。
- 先把响应落到文件里，再回头看。边收边 grep 的管道，会把你十分钟后想
  查的证据当场扔掉。
- 流式加 `--no-buffer` 抓，每个 SSE 帧都留着，看着像噪声的也留。
  `message_start` 之前、`message_stop` 之后冒出来的 `ping` 帧，本身就是
  一条实打实的发现。
- 失败路径要专门去踩：模型 id 写错、key 写错、请求体故意畸形、漏掉必填
  字段、把 `max_tokens` 压低到能把工具调用的参数截在一半。两个协议差得
  最远的地方就是失败的形状，也正是文档最不靠谱的地方。

## 记录

往 `docs/wire-notes.md` 里加一节，编号排上，内容要有：

- 原封不动的那条 `curl` 命令，key 打码。
- 原始响应体或者 SSE 帧，逐字照抄，只在明确标注的地方省略。
- 一句话说清这条发现是什么。
- 跟协议文档矛盾的话，补一条注记，把文档的说法一并写上。

绝不要凭着对某份规范的记忆写论断。论断没带着证据进 wire-notes，就不许
进任何一章。
