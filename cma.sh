# claude-multi-acct (cma) — 官网多账号并行 / 轮询工具
# ------------------------------------------------------------------
# 机制(已从 claude 二进制反查确认, v2.1.216):
#   凭证服务名 = "Claude Code-credentials" + (CLAUDE_CONFIG_DIR 未设时为空
#                                            / 设了则加 "-<sha256(dir)[:8]>")
#   -> 每个 CLAUDE_CONFIG_DIR 自动拿到一条独立 Keychain 凭证, 天然隔离, 可并行。
#   -> 不设 CLAUDE_CONFIG_DIR 的默认账号(现有的 `cc`) 永远是 "Claude Code-credentials",
#      本工具任何代码路径都不会去写它 —— 这就是"垫底"的结构性保证。
#
# 铁律: 任何解析失败 -> unset CLAUDE_CONFIG_DIR -> 回落默认账号, 绝不留下损坏状态。
# ------------------------------------------------------------------

# 账号数据根目录 (每个子目录 = 一个账号的 CLAUDE_CONFIG_DIR)
: "${CMA_HOME:=$HOME/Project/claude-multi-acct/accounts}"

# 启动 claude 时附带的参数 (与现有 cc 保持一致)
: "${CMA_CLAUDE_FLAGS:=--dangerously-skip-permissions --remote-control}"

_cma_reserved="add rm ls list next link unlink help -h --help"

# 账号里某项被软链前的备份后缀
_cma_bak_suffix=".isolated"
# 全局配置目录 (软链目标)
: "${CMA_GLOBAL_DIR:=$HOME/.claude}"

# [全局]模式软链哪些"具体子项"到全局 (纯资产/历史, 无账号身份)。
# 可用环境变量覆盖增减。铁律: .claude.json / .credentials.json 永不软链,
# 它们含 oauthAccount 登录态与凭证, 软链会导致多账号串号 —— 这正是之前"登录态丢失"的根因。
: "${CMA_GLOBAL_LINKS:=settings.json CLAUDE.md skills plugins commands agents sessions projects todos}"
_cma_never_link=".claude.json .credentials.json"

# 清空第三方 provider 相关环境变量 (避免污染官网模式)
_cma_clear_env() {
  unset ANTHROPIC_BASE_URL ANTHROPIC_AUTH_TOKEN ANTHROPIC_API_KEY \
        ANTHROPIC_MODEL ANTHROPIC_SMALL_FAST_MODEL ANTHROPIC_DEFAULT_MODEL \
        ANTHROPIC_DEFAULT_OPUS_MODEL ANTHROPIC_DEFAULT_SONNET_MODEL \
        ANTHROPIC_DEFAULT_HAIKU_MODEL CLAUDE_CODE_OAUTH_TOKEN \
        CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY 2>/dev/null
}

# 回落默认账号: 不设 CLAUDE_CONFIG_DIR, 走现有那条永不被碰的凭证
_cma_run_default() {
  _cma_clear_env
  unset CLAUDE_CONFIG_DIR
  command claude $CMA_CLAUDE_FLAGS "$@"
}

# 账号目录绝对路径 (统一 normalize, 保证与 keychain hash 一致)
_cma_dir() { printf '%s/%s' "$CMA_HOME" "$1"; }

# 校验账号名: 只允许 [A-Za-z0-9._-], 且不能是保留词
_cma_valid_name() {
  local n="$1"
  case " $_cma_reserved " in *" $n "*) return 2 ;; esac
  [[ "$n" =~ ^[A-Za-z0-9._-]+$ ]] || return 1
  return 0
}

# 已从二进制确认: keychain 服务名 = "Claude Code-credentials-<sha256(abspath)[:8]>"
_cma_kc_service() {
  local dir="$1" h
  h=$(printf '%s' "$dir" | shasum -a 256 2>/dev/null | cut -c1-8)
  [ -n "$h" ] && printf 'Claude Code-credentials-%s' "$h"
}

# 尽力检测某账号是否已登录 (非关键路径, 仅供 ls 展示)
_cma_logged_in() {
  local dir="$1" svc
  [ -f "$dir/.credentials.json" ] && return 0
  svc=$(_cma_kc_service "$dir")
  [ -n "$svc" ] && security find-generic-password -s "$svc" -w >/dev/null 2>&1
}

# 从 <dir>/.claude.json 读登录邮箱 (读不到返回 '-'); 用环境变量传路径, 防特殊字符
_cma_email() {
  local f="$1/.claude.json"
  [ -f "$f" ] || { printf '%s' '-'; return; }
  CMA_JSON="$f" python3 -c "import json,os
try:
    d=json.load(open(os.environ['CMA_JSON']))
    print((d.get('oauthAccount') or {}).get('emailAddress') or '-')
except Exception:
    print('-')" 2>/dev/null || printf '%s' '-'
}

# 启动指定账号 (核心)
_cma_launch() {
  local name="$1"; shift
  local dir
  dir="$(_cma_dir "$name")"
  if [ -z "$name" ] || [ ! -d "$dir" ]; then
    echo "cc2: 未知账号 '$name' —— 回落默认账号(垫底)" >&2
    _cma_run_default "$@"
    return
  fi
  _cma_clear_env
  # env 隔离: 只对本次 claude 进程生效, 不污染当前 shell
  env CLAUDE_CONFIG_DIR="$dir" command claude $CMA_CLAUDE_FLAGS "$@"
}

# 新增账号 + 交互登录
_cma_add() {
  local name="$1"; shift
  if [ -z "$name" ]; then echo "用法: cc2 add <账号名> [--global] [claude参数...]" >&2; return 1; fi
  local global=0
  if [ "$1" = "--global" ]; then global=1; shift; fi
  if ! _cma_valid_name "$name"; then
    echo "cc2: 非法账号名 '$name' (只能用 [A-Za-z0-9._-], 且不能是保留词: $_cma_reserved)" >&2
    return 1
  fi
  local dir; dir="$(_cma_dir "$name")"
  if [ -e "$dir" ] || [ -L "$dir" ]; then echo "cc2: 账号 '$name' 已存在, 用 cc2 rm 先删或换名" >&2; return 1; fi
  mkdir -p "$dir" || { echo "cc2: 无法创建 $dir" >&2; return 1; }
  if [ "$global" = 1 ]; then
    echo "▶ 账号 '$name' [全局]: 软链共享子项 -> $CMA_GLOBAL_DIR (.claude.json/凭证仍独立)"
    _cma_link_items "$dir"
  else
    echo "▶ 账号 '$name' [独立]: $dir"
  fi
  echo "▶ 即将启动交互登录。若没自动弹出登录, 在会话里输入  /login  即可。登录完成后退出即可用 cc2 $name 启动。"
  _cma_clear_env
  # 登录也带上与 cc 一致的默认参数 (--dangerously-skip-permissions --remote-control)
  env CLAUDE_CONFIG_DIR="$dir" command claude $CMA_CLAUDE_FLAGS "$@"
}

# 删除账号目录 (只删本工具管理的目录, 从不碰默认 ~/.claude)
_cma_rm() {
  local name="$1"
  if [ -z "$name" ]; then echo "用法: cc2 rm <账号名>" >&2; return 1; fi
  local dir bak; dir="$(_cma_dir "$name")"; bak="${dir}${_cma_bak_suffix}"
  if [ ! -e "$dir" ] && [ ! -L "$dir" ]; then echo "cc2: 账号 '$name' 不存在" >&2; return 1; fi
  # 安全护栏: 绝不删到 CMA_HOME 之外或默认配置目录
  case "$dir" in
    "$CMA_HOME"/*) : ;;
    *) echo "cc2: 拒绝删除非账号目录 $dir" >&2; return 1 ;;
  esac
  if [ -L "$dir" ]; then
    rm -f "$dir" && echo "已删除账号 '$name' (全局软链, 未触碰 $CMA_GLOBAL_DIR)"
  else
    rm -rf "$dir" && echo "已删除账号 '$name' ($dir)"
  fi
  # 顺带清理独立备份
  if [ -d "$bak" ]; then rm -rf "$bak" && echo "已清理备份 $bak"; fi
  echo "提示: keychain 里对应凭证条目未删, 如需彻底清理:"
  echo "  security delete-generic-password -s \"$(_cma_kc_service "$dir")\" 2>/dev/null"
}

# 列出所有账号 + 登录状态
_cma_ls() {
  mkdir -p "$CMA_HOME"
  local any=0 d base name status mode email
  echo "账号根目录: $CMA_HOME"
  printf "  %-14s %-6s %-30s %s\n" "默认(垫底)" "-" "$(_cma_email "$HOME")" "无 CLAUDE_CONFIG_DIR, 本工具永不修改"
  echo "  --------------------------------------------------------------------"
  printf "  %-14s %-6s %-30s %s\n" "账号" "模式" "登录邮箱" "凭证"
  echo "  --------------------------------------------------------------------"
  for d in "$CMA_HOME"/*/; do
    [ -e "$d" ] || continue
    base="${d%/}"; name="$(basename "$base")"
    case "$name" in *"$_cma_bak_suffix") continue ;; esac   # 跳过独立备份
    any=1
    if _cma_is_global "$base"; then mode="[全局]"; else mode="[独立]"; fi
    if _cma_logged_in "$base"; then status="✓ 已登录"; else status="· 未登录"; fi
    printf "  %-14s %-6s %-30s %s\n" "$name" "$mode" "$(_cma_email "$base")" "$status"
  done
  [ "$any" = 0 ] && echo "  (还没有账号, 用 cc2 add <名字> 添加)"
  return 0
}

# 轮询: 按排序选下一个账号, 游标存 CMA_HOME/.rotation
_cma_next() {
  mkdir -p "$CMA_HOME"
  local list=() d nm
  for d in "$CMA_HOME"/*/; do
    [ -e "$d" ] || continue
    nm="$(basename "${d%/}")"
    case "$nm" in *"$_cma_bak_suffix") continue ;; esac   # 跳过独立备份
    list+=("$nm")
  done
  if [ "${#list[@]}" -eq 0 ]; then
    echo "cc2: 还没有任何账号 —— 回落默认账号(垫底)" >&2
    _cma_run_default "$@"
    return
  fi
  # 排序保证轮询顺序稳定
  IFS=$'\n' list=($(printf '%s\n' "${list[@]}" | sort)); unset IFS
  local cursor=0 rot="$CMA_HOME/.rotation"
  [ -f "$rot" ] && cursor=$(cat "$rot" 2>/dev/null)
  [[ "$cursor" =~ ^[0-9]+$ ]] || cursor=0
  local idx=$(( cursor % ${#list[@]} ))
  local pick="${list[$idx]}"
  printf '%s' "$(( (idx + 1) % ${#list[@]} ))" > "$rot" 2>/dev/null
  echo "▶ 轮询选中账号: $pick  (第 $((idx+1))/${#list[@]} 个)"
  _cma_launch "$pick" "$@"
}

# 在账号目录里, 把 CMA_GLOBAL_LINKS 列出的"具体子项"逐个软链到全局。
# 只软链全局里真实存在的项; 账号里已有的同名项先改名备份为 <item>.isolated。
# .claude.json / .credentials.json 永不软链 (保留账号独立登录态)。
_cma_link_items() {
  local dir="$1" item src dst n=0
  for item in $CMA_GLOBAL_LINKS; do
    case " $_cma_never_link " in *" $item "*) continue ;; esac   # 双保险: 绝不软链身份/凭证
    src="$CMA_GLOBAL_DIR/$item"; dst="$dir/$item"
    [ -e "$src" ] || continue                 # 全局没有的项跳过
    [ -L "$dst" ] && continue                 # 已是软链
    if [ -e "${dst}${_cma_bak_suffix}" ]; then
      echo "  跳过 $item (备份 ${item}${_cma_bak_suffix} 已存在, 请手动处理)"; continue
    fi
    if [ -e "$dst" ]; then mv "$dst" "${dst}${_cma_bak_suffix}" || continue; fi
    ln -s "$src" "$dst" && { n=$((n+1)); echo "  link: $item -> $src"; }
  done
  echo "  共链接 $n 项; .claude.json / .credentials.json 保持账号独立(不共享登录态)"
}

# 切到[全局]: 软链具体子项 (不再整目录软链)。
_cma_link() {
  local name="$1"
  if [ -z "$name" ]; then echo "用法: cc2 link <账号名>" >&2; return 1; fi
  local dir; dir="$(_cma_dir "$name")"
  if [ -L "$dir" ]; then
    echo "cc2: '$name' 是旧版整目录软链(有串号风险), 请先 cc2 unlink $name 修复再 link" >&2; return 1
  fi
  if [ ! -d "$dir" ]; then echo "cc2: 账号 '$name' 不存在, 先 cc2 add $name" >&2; return 1; fi
  echo "账号 '$name' -> [全局]"
  _cma_link_items "$dir"
}

# 切回[独立]: 只删指向全局的软链, 再从备份恢复账号自己的子项。
_cma_unlink() {
  local name="$1"
  if [ -z "$name" ]; then echo "用法: cc2 unlink <账号名>" >&2; return 1; fi
  local dir; dir="$(_cma_dir "$name")"
  # 兼容历史: 旧版整目录软链
  if [ -L "$dir" ]; then
    local bak="${dir}${_cma_bak_suffix}"
    rm -f "$dir"                              # 只删软链, 不碰 $CMA_GLOBAL_DIR
    if [ -d "$bak" ]; then mv "$bak" "$dir"; else mkdir -p "$dir"; fi
    echo "账号 '$name' 已从旧版整目录软链恢复[独立]"; return 0
  fi
  if [ ! -d "$dir" ]; then echo "cc2: 账号 '$name' 不存在" >&2; return 1; fi
  local item dst n=0
  for item in $CMA_GLOBAL_LINKS; do
    dst="$dir/$item"
    [ -L "$dst" ] || continue
    case "$(readlink "$dst")" in "$CMA_GLOBAL_DIR"/*) : ;; *) continue ;; esac   # 只解本工具建的全局软链
    rm -f "$dst"                              # 只删软链本身
    [ -e "${dst}${_cma_bak_suffix}" ] && mv "${dst}${_cma_bak_suffix}" "$dst"
    n=$((n+1)); echo "  unlink: $item"
  done
  echo "账号 '$name' -> [独立] (恢复 $n 项)"
}

# 账号是否处于[全局]: 目录内存在指向全局的软链, 或是旧版整目录软链
_cma_is_global() {
  local dir="$1" item
  [ -L "$dir" ] && return 0
  for item in $CMA_GLOBAL_LINKS; do [ -L "$dir/$item" ] && return 0; done
  return 1
}

_cma_help() {
  cat <<'EOF'
cc2 — 官网多账号并行 / 轮询 (默认账号永远垫底, 不会被本工具修改)

  cc2 add <名字> [--global]  新增账号并交互登录; --global 直接建成共享全局设置
  cc2 <名字> [参数...]        以该账号启动 claude (参数原样透传, 如 --resume)
  cc2 next [参数...]          轮询: 自动挑下一个账号启动, 均摊用量
  cc2 link <名字>            切到[全局]: 把 skills/plugins/settings 等具体子项软链到 ~/.claude
  cc2 unlink <名字>          切回[独立]: 删除这些软链, 从备份恢复账号自己的子项
  cc2 ls                    列出所有账号 / [独立]或[全局] / 登录邮箱 / 凭证状态
  cc2 rm <名字>             删除某账号目录 (从不碰 ~/.claude)
  cc2 help                  本帮助

说明:
  * 每个账号是一个 CLAUDE_CONFIG_DIR 目录, 凭证由 claude 按目录路径 hash 隔离,
    因此不同账号可在多个终端"同时并行跑", 各自消耗各自用量。
  * [全局]模式: 只把 CMA_GLOBAL_LINKS 里的"具体子项"(skills/plugins/settings/
    sessions 等)软链到 ~/.claude, 共享全局设置; .claude.json(登录态) 和
    .credentials.json(凭证) 永不软链 —— 各账号登录态完全独立, 绝不串号。
  * 现有的 cc / ccr / ccl 完全不受影响。
  * 任何解析失败一律回落默认账号, 不会损坏或误改任何凭证。
EOF
}

# 主入口: 第一个参数是动词, 否则当作账号名
cc2() {
  local verb="$1"
  case "$verb" in
    add)        shift; _cma_add "$@" ;;
    rm)         shift; _cma_rm "$@" ;;
    ls|list)    _cma_ls ;;
    next)       shift; _cma_next "$@" ;;
    link)       shift; _cma_link "$@" ;;
    unlink)     shift; _cma_unlink "$@" ;;
    help|-h|--help|"") _cma_help ;;
    *)          shift; _cma_launch "$verb" "$@" ;;
  esac
}
