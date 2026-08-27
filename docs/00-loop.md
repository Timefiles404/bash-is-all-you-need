# 第 00 阶段 — 主循环

**你要构建什么**：一个能探索代码库、运行命令、编辑文件、检查自己工作成果的 coding Agent。

**它要花你什么**：一个文件，约 200 行 Go 代码，不需要标准库之外的依赖。

这个比例就是第一课。Agent 不是一个大软件。这个仓库所有的硬活都在这一阶段**之后**的阶段里，都不是关于让 Agent 更聪明。

---

## 按这个顺序读

`stages/00-loop/main.go`，三个函数：

1. **`main()`** — 主循环。先读这个；其他一切都是它调用的细节。
2. **`callModel()`** — 一个 HTTP POST。这里没有 SDK，SDK 下面也没有任何东西，只有这个。
3. **`runBash()`** — 十行。Agent 能做的每一个动作都通过它们。

## 主循环的形状

```
user types a task
  └─ append {role:"user"} to messages
     └─ LOOP:
        ├─ POST /chat/completions with the whole message array
        ├─ append the assistant reply to messages
        ├─ no tool_calls?  → print the text, exit LOOP
        └─ for each tool_call:
             run the command
             append {role:"tool", tool_call_id, content: output}
           → back to the top
```

撑住它的是两个不变量，两个都是承重的：

**消息数组只增不减。** 永远不编辑、不删除任何东西。对话**就是** Agent 的记忆，每一个请求都重新发送整个历史。第 05 阶段是第一次我们触碰这个规则，打破它会有代价。

**每个工具调用都必须得到一个结果。** 如果模型要求三个命令而你只回答两个，下一个请求格式就错了。如果一个命令失败了，失败**本身就是**结果——见下面。

---

## 错误是观察，不是异常

`runBash` 里最常见的初学者错误是这样的：

```go
out, err := cmd.CombinedOutput()
if err != nil {
    return err          // ← 错了
}
```

一个以非零退出码结束的命令没有破坏你的程序。它生成了信息，而模型才是应该对它做出反应的组件。看看下面真实运行里发生了什么：`python stats.py` 退出码 1 带着 `ZeroDivisionError` 回溯，我们原样递交回溯，模型用它找到了 bug。如果我们在那里返回了 Go 错误，Agent 就会在它变得有用的地方停下来。

同样的本能一路向上贯穿整个系统：你的工作是忠实地向模型报告世界，不是保护模型不去接触它。

---

## 实验：看账单长大

设置一个临时目录，在一个它得自己找出来的 bug 上运行 Agent。

```sh
go build -o agent ./stages/00-loop
mkdir -p sandbox && cd sandbox
# ... put some broken code here ...
export AGENT_BASE_URL=... AGENT_API_KEY=... AGENT_MODEL=...
../agent
> There is a bug in this directory's code. Find it, fix it, and verify the fix.
```

这是实际运行六个回合（找 bug → 读文件 → 运行 → 补丁 → 重读 → 验证）的 token 行：

| 回合 | 它做了什么 | prompt token 数 |
|---:|---|---:|
| 1 | `ls -la` | 429 |
| 2 | `cat README.md && cat stats.py` | 613 |
| 3 | `python stats.py` → 回溯 | 737 |
| 4 | `sed -i ...` 补丁 | 932 |
| 5 | `cat stats.py` | 1079 |
| 6 | `python stats.py` → 干净 | 1192 |

现在把右边那列加起来：**4982 个 token 被计费**，而对话最终大小是**1192 个 token**。我们为我们构建的东西付了 4.2 倍的价格。

这不是代码里的 bug。它就是"每次回合重新发送整个历史"的意思，而且是平方的：一个 40 回合的会话为早期回合每一个都付费 40 次。每个严肃的 Agent 都有它的答案。我们的答案在**第 04 阶段**，同一个实验运行两次——一次用缓存，一次用 `--break-cache`——把差异用数字表现出来。

保留这张表。它是这个仓库其余部分的基准。

---

## 故意缺失的东西

每一个都是第 01 阶段的话题。在读修复前试着触发它们——失败比补丁更容易记住。

| 缺失的 | 怎么让它咬人 |
|---|---|
| 输出截断 | 要它 `find /` 或读一个大文件。看上下文窗口填满噪音。 |
| 命令超时 | 要它启动一个开发服务器。Agent 永远挂起。 |
| 进程树杀死 | 即使有超时，一个被杀死的 shell 也能留下孤立的子进程。 |
| 权限闸 | 没什么阻止 `rm -rf`。这是你为什么用临时目录。 |
| 流式 | 你整个模型回合都瞪着空白终端。 |
| `finish_reason` 处理 | 我们只在"有没有工具调用"上分支。`length`（击中 `max_tokens`）被无声地当作一个完成的回合。 |

`maxTurns` 保险丝是唯一及早加入的保命特性，因为没有它，一个陷入重试循环的模型会在你读这个文件的时候烧掉你的 key。

---

## 线上的备注

真实 API 做过的、`main.go` 里的类型得配上的事情：

- 在工具调用回合上，**`content` 回来的是 `null`**，不是 `""`。Go 的 `json.Unmarshal` 把 null 当作无操作，所以一个普通的 `string` 字段能活过它——但一个有更严格 null 处理的语言会在这里崩溃。
- **`tool_calls[].function.arguments` 是一个 JSON *字符串*，不是一个对象。** 你必须解析它。永远不要用字符串匹配它。
- **`reasoning_content` 在会思考的模型上出现。** 我们在这个阶段扔掉它；第 02 阶段渲染它，第 03 阶段展示它怎么映射到 Anthropic 协议的 `thinking` 块。

---

## 练习

1. **破坏工具调用配对。** 跳过附加一个 `role:"tool"` 消息，读 API 返回的错误。这比文档更能教你关于协议的东西。
2. **删除系统提示词。** 看 Agent 的行为如何降级——它开始问**你**去运行命令。大部分"Agent 质量"住在那个字符串里。
3. **把 `maxTokens` 改成 100。** 现在 `finish_reason` 回来的是 `length`，Agent 无声地在想法中途截断。这是第 01 阶段修复的 bug。
4. **指向一个不同的模型。** 任何 OpenAI 兼容的端点都行，包括一个本地的 Ollama。小模型生成格式错乱的工具调用——那不是你的代码失败，早期看见它是值得的。

→ 下一步：[第 01 阶段 — 别死掉](01-dont-die.md)
