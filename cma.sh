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

# 独立目录被 link 到全局前的备份后缀
_cma_bak_suffix=".isolated"
# 全局配置目录 (link 的目标)
: "${CMA_GLOBAL_DIR:=$HOME/.claude}"

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
  if [ "$global" = 1 ]; then
    ln -s "$CMA_GLOBAL_DIR" "$dir" || { echo "cc2: 无法建立全局软链 $dir" >&2; return 1; }
    echo "▶ 账号 '$name' [全局]: $dir -> $CMA_GLOBAL_DIR (共享全局设置; 凭证仍按独立 keychain 条目, 需单独登录)"
  else
    mkdir -p "$dir" || { echo "cc2: 无法创建 $dir" >&2; return 1; }
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
  local any=0 d base name status mode
  echo "账号根目录: $CMA_HOME"
  echo "默认账号(垫底): 无 CLAUDE_CONFIG_DIR -> 'Claude Code-credentials' (本工具永不修改)"
  echo "----------------------------------------------"
  for d in "$CMA_HOME"/*/; do
    [ -e "$d" ] || continue
    base="${d%/}"; name="$(basename "$base")"
    case "$name" in *"$_cma_bak_suffix") continue ;; esac   # 跳过独立备份
    any=1
    if [ -L "$base" ]; then mode="[全局]"; else mode="[独立]"; fi
    if _cma_logged_in "$base"; then status="✓ 已登录"; else status="· 未登录"; fi
    printf "  %-18s %-6s %s\n" "$name" "$mode" "$status"
  done
  [ "$any" = 0 ] && echo "  (还没有账号, 用 cc2 add <名字> 添加)"
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

# 切到[全局]: 整个账号目录软链到 CMA_GLOBAL_DIR。链接前把独立目录改名备份。
_cma_link() {
  local name="$1"
  if [ -z "$name" ]; then echo "用法: cc2 link <账号名>" >&2; return 1; fi
  local dir bak; dir="$(_cma_dir "$name")"; bak="${dir}${_cma_bak_suffix}"
  if [ -L "$dir" ]; then echo "账号 '$name' 已是[全局]模式"; return 0; fi
  if [ ! -d "$dir" ]; then echo "cc2: 账号 '$name' 不存在, 先 cc2 add $name" >&2; return 1; fi
  if [ -e "$bak" ]; then echo "cc2: 备份 $bak 已存在, 请先手动处理" >&2; return 1; fi
  mv "$dir" "$bak" || { echo "cc2: 备份失败" >&2; return 1; }
  if ! ln -s "$CMA_GLOBAL_DIR" "$dir"; then
    mv "$bak" "$dir"   # 回滚, 保证不留下损坏状态
    echo "cc2: 建立软链失败, 已回滚为独立目录" >&2; return 1
  fi
  echo "账号 '$name' -> [全局]  $dir -> $CMA_GLOBAL_DIR"
  echo "  独立配置已备份到: $bak (cc2 unlink $name 可恢复)"
}

# 切回[独立]: 只删软链本身(绝不 rm -rf 目标), 再从备份恢复独立目录。
_cma_unlink() {
  local name="$1"
  if [ -z "$name" ]; then echo "用法: cc2 unlink <账号名>" >&2; return 1; fi
  local dir bak; dir="$(_cma_dir "$name")"; bak="${dir}${_cma_bak_suffix}"
  if [ ! -L "$dir" ]; then
    [ -d "$dir" ] && { echo "账号 '$name' 已是[独立]模式"; return 0; }
    echo "cc2: 账号 '$name' 不存在" >&2; return 1
  fi
  rm -f "$dir" || { echo "cc2: 删除软链失败" >&2; return 1; }   # 只删链接, 不碰 $CMA_GLOBAL_DIR
  if [ -d "$bak" ]; then
    mv "$bak" "$dir" && echo "账号 '$name' -> [独立]  已从备份恢复 ($bak)"
  else
    mkdir -p "$dir" && echo "账号 '$name' -> [独立]  无备份, 已重建空目录(首次启动走登录)"
  fi
}

_cma_help() {
  cat <<'EOF'
cc2 — 官网多账号并行 / 轮询 (默认账号永远垫底, 不会被本工具修改)

  cc2 add <名字> [--global]  新增账号并交互登录; --global 直接建成共享全局设置
  cc2 <名字> [参数...]        以该账号启动 claude (参数原样透传, 如 --resume)
  cc2 next [参数...]          轮询: 自动挑下一个账号启动, 均摊用量
  cc2 link <名字>            切到[全局]: 整个账号目录软链到 ~/.claude (链接前自动备份独立目录)
  cc2 unlink <名字>          切回[独立]: 断开软链, 从备份恢复独立目录
  cc2 ls                    列出所有账号 / [独立]或[全局] / 登录状态
  cc2 rm <名字>             删除某账号目录 (软链只删链接, 从不碰 ~/.claude)
  cc2 help                  本帮助

说明:
  * 每个账号是一个 CLAUDE_CONFIG_DIR 目录, 凭证由 claude 按目录路径 hash 隔离,
    因此不同账号可在多个终端"同时并行跑", 各自消耗各自用量。
  * [全局]模式: 账号目录软链到 ~/.claude, 共享全局设置/skills/plugins;
    凭证仍按独立 keychain 条目隔离(路径 hash 不解析软链), 需各自登录。
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
