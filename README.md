# claude-multi-acct (cc2)

官网 **多账号并行 / 轮询** 工具。核心目标：让多个 Claude 官网账号能**同时并行跑**、均摊用量，同时保证**当前账号永远垫底**——本工具任何代码路径都不会修改默认账号凭证，出任何问题都回落到默认账号。

## 为什么这样就够了（机制，反查自 `claude` 二进制 v2.1.216）

Claude Code 的官网凭证服务名由这段逻辑决定：

```js
// 服务名 = "Claude Code-credentials" + (设了 CLAUDE_CONFIG_DIR 则追加 "-<sha256(dir)[:8]>")
r = !process.env.CLAUDE_CONFIG_DIR;
o = r ? "" : `-${sha256(configDir).slice(0,8)}`;
return `Claude Code-credentials${o}`;
```

读取顺序 `pwc(keychain, plaintext)`：先读 Keychain，读不到再回退读该配置目录下的 `.credentials.json`。

结论：

| 场景 | Keychain 条目 | 隔离性 |
|---|---|---|
| 不设 `CLAUDE_CONFIG_DIR`（现有 `cc`） | `Claude Code-credentials` | **默认账号，本工具永不修改** |
| `CLAUDE_CONFIG_DIR=…/accounts/foo` | `Claude Code-credentials-<hash>` | 独立凭证，可并行 |

所以**一个 `CLAUDE_CONFIG_DIR` 目录 = 一个账号**，凭证隔离由 claude 自己按目录 hash 完成，天生支持多终端同时跑。

## 架构

```mermaid
flowchart TD
    U[用户命令 cc2] --> D{第一个参数}
    D -->|add name| ADD[建目录 + 交互 /login]
    D -->|name| L[启动指定账号]
    D -->|next| R[轮询挑下一个账号]
    D -->|ls / rm / help| M[管理]

    L --> CK{目录存在?}
    CK -->|否/空| FB[unset CLAUDE_CONFIG_DIR<br/>回落默认账号 垫底]
    CK -->|是| RUN[env CLAUDE_CONFIG_DIR=dir claude]

    R --> POOL{有账号?}
    POOL -->|否| FB
    POOL -->|是| L

    RUN --> KC[(Keychain<br/>Claude Code-credentials-hash)]
    FB --> KD[(Keychain<br/>Claude Code-credentials<br/>默认账号 永不被写)]

    style FB fill:#fde,stroke:#c33
    style KD fill:#dfd,stroke:#3a3
```

## 安装

已自动接入 `~/.bashrc`（幂等，带 `>>> claude-multi-acct` 标记）。新开终端或：

```bash
source ~/.bashrc
```

## 用法

```bash
cc2 add work2            # 新增账号 work2 并交互登录 (/login)
cc2 add work3 --global   # 新增并直接共享全局设置(选择性软链具体子项)
cc2 work2                # 以 work2 账号启动 claude
cc2 work2 --resume       # 参数原样透传给 claude
cc2 next                 # 轮询: 自动挑下一个账号启动, 均摊用量
cc2 link work2           # 切到[全局]: 软链 skills/plugins/settings 等子项到 ~/.claude
cc2 unlink work2         # 切回[独立]: 断开软链, 从备份恢复
cc2 set work2 skip on    # 打开 --dangerously-skip-permissions (默认关)
cc2 set work2 rc on      # 打开 --remote-control (默认关)
cc2 set default skip on  # 也可给垫底账号设开关
cc2 ls                   # 列出账号 / 模式 / 启动参数开关 / 登录邮箱 / 凭证
cc2 rm work2             # 删除账号 (从不碰 ~/.claude)
cc2 help
```

## 启动参数开关

`--dangerously-skip-permissions` 和 `--remote-control` 是**每账号独立的开关，默认全部关闭**，用 `cc2 set` 打开/关闭，`cc2 ls` 可查看：

```bash
cc2 set <账号|default> skip on|off   # skip = --dangerously-skip-permissions
cc2 set <账号|default> rc   on|off   # rc   = --remote-control
```

- 状态用账号目录里的标记文件记录（`.cma-flag-skip` / `.cma-flag-rc`），不进 git、不进软链清单。
- `default` 指垫底账号（回落时使用）。
- 启动时按开关动态拼接参数；都关则不带任何危险参数（最安全默认）。

并行跑：在不同终端分别 `cc2 alpha`、`cc2 beta`、`cc2 gamma`，互不干扰，各耗各的用量。

## 独立 / 全局 两种模式

每个账号可在两种模式间切换。**凭证与登录态在两种模式下都完全独立**（keychain 按 `CLAUDE_CONFIG_DIR` 路径字符串 hash；而 `.claude.json`/`.credentials.json` 永不软链）：

- **[独立]**（默认）：`accounts/<name>` 是普通目录，配置/skills/plugins 都是这个账号自己的。
- **[全局]**：只把 **具体子项** 软链到 `~/.claude`，共享你精心配置的全局资产；`.claude.json`（登录身份）和 `.credentials.json`（凭证）留在账号自己目录里。

默认软链清单（环境变量 `CMA_GLOBAL_LINKS` 可增减）：

```
settings.json  CLAUDE.md  skills  plugins  commands  agents  sessions  projects  todos
```

> ⚠️ **为什么不整目录软链？** `.claude.json` 含 `oauthAccount`（登录身份），且它不在 `~/.claude/` 里而在 `~/.claude.json`。若整目录软链到 `~/.claude`，多个全局账号会共写同一个 `~/.claude/.claude.json` → **登录态互相覆盖 / 串号**。所以只软链无身份的资产，`.claude.json`/`.credentials.json` 由 `_cma_never_link` 强制排除，即便手动塞进清单也不会被软链。

切换语义（完全可逆）：

```mermaid
stateDiagram-v2
    [*] --> 独立: cc2 add name
    独立 --> 全局: cc2 link name<br/>(逐个子项: 账号自己的改名备份为 item.isolated,<br/>再软链全局同名项)
    全局 --> 独立: cc2 unlink name<br/>(只删指向全局的软链,<br/>再从 item.isolated 恢复)
    独立 --> 全局2: cc2 add name --global
    全局2: 全局
    note right of 全局
        .claude.json / .credentials.json 永不软链,
        各账号登录态独立, 绝不串号
    end note
```

## 安全保证（垫底）

- 本工具**仅在设置了 `CLAUDE_CONFIG_DIR` 时工作**，而默认账号凭证在 `Claude Code-credentials`（无后缀）这条谁都不去写的条目里——结构性隔离，不靠代码小心。
- 任何解析失败（未知账号、空账号池）一律 `unset CLAUDE_CONFIG_DIR` → 回落默认账号。
- 现有 `cc` / `ccr` / `ccl` / `cclr` 完全不受影响。
- `cc2 rm` 有护栏，只删 `accounts/` 内目录，绝不误删 `~/.claude`。
- `cc2 link` 逐个子项处理：账号自己的同名项先改名备份为 `<item>.isolated`，再软链全局同名项；`cc2 unlink` 只删指向全局的软链（`rm -f`，绝不 `rm -rf` 到目标），再从备份恢复。全程不复制/删除 `~/.claude` 内容。
- `.claude.json`（登录身份）与 `.credentials.json`（凭证）由 `_cma_never_link` 强制排除，任何模式下都各账号独立，绝不串号。

## 配置项（环境变量，可选）

| 变量 | 默认 | 说明 |
|---|---|---|
| `CMA_HOME` | `~/Project/claude-multi-acct/accounts` | 账号数据根目录 |
| `CMA_CLAUDE_FLAGS` | `--dangerously-skip-permissions --remote-control` | 启动 claude 的固定参数，与 `cc` 一致；`cc2 <name>` / `next` / `add` 全部带上 |

## 备注

- `cc2 ls` 的"已登录"检测按二进制里的 hash 算法计算 keychain 服务名，属尽力而为、仅供展示，不影响启动。
- 卸载：删掉 `~/.bashrc` 里 `>>> claude-multi-acct` 标记块即可（备份见 `~/.bashrc.bak.cma.*`）。
