// cc2 — 官网多账号并行 / 轮询工具 (Go 重写版)
//
// 机制(从 claude 二进制反查确认, v2.1.216):
//   凭证服务名 = "Claude Code-credentials" + (未设 CLAUDE_CONFIG_DIR 时为空
//                                            / 设了则加 "-<sha256(dir)[:8]>")
//   -> 每个 CLAUDE_CONFIG_DIR 拿到独立 Keychain 凭证, 天然隔离, 可并行。
//   -> 不设 CLAUDE_CONFIG_DIR 的默认账号(cc)永远是 "Claude Code-credentials",
//      本工具任何路径都不写它 —— "垫底"的结构性保证。
//
// 铁律: 任何解析失败 -> 不设 CLAUDE_CONFIG_DIR -> 回落默认账号, 绝不留下损坏状态。
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// ---------- 配置 ----------

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func cmaHome() string   { return env("CMA_HOME", filepath.Join(home(), ".cc2")) }
func globalDir() string { return env("CMA_GLOBAL_DIR", filepath.Join(home(), ".claude")) }

func globalLinks() []string {
	return strings.Fields(env("CMA_GLOBAL_LINKS",
		"settings.json CLAUDE.md skills plugins commands agents sessions projects todos"))
}

// 永不软链: 含登录身份/凭证, 软链会导致多账号串号
var neverLink = map[string]bool{".claude.json": true, ".credentials.json": true}

// 启动 claude 永远附带的额外参数(默认空; 逃生阀)
func extraFlags() []string { return strings.Fields(os.Getenv("CMA_CLAUDE_FLAGS")) }

var providerVars = map[string]bool{
	"ANTHROPIC_BASE_URL": true, "ANTHROPIC_AUTH_TOKEN": true, "ANTHROPIC_API_KEY": true,
	"ANTHROPIC_MODEL": true, "ANTHROPIC_SMALL_FAST_MODEL": true, "ANTHROPIC_DEFAULT_MODEL": true,
	"ANTHROPIC_DEFAULT_OPUS_MODEL": true, "ANTHROPIC_DEFAULT_SONNET_MODEL": true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
	"CLAUDE_CODE_USE_BEDROCK": true, "CLAUDE_CODE_USE_VERTEX": true, "CLAUDE_CODE_USE_FOUNDRY": true,
}

var reserved = map[string]bool{
	"add": true, "rm": true, "ls": true, "list": true, "next": true,
	"link": true, "unlink": true, "set": true, "default": true,
	"help": true, "-h": true, "--help": true,
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ---------- 小工具 ----------

func accountDir(name string) string { return filepath.Join(cmaHome(), name) }

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }
func isDir(p string) bool  { fi, err := os.Stat(p); return err == nil && fi.IsDir() }
func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cc2: "+format+"\n", a...)
	os.Exit(1)
}

// 校验账号名: 合法字符 + 非保留词 + 无路径穿越
func validName(name string) error {
	if name == "" {
		return fmt.Errorf("账号名不能为空")
	}
	if reserved[name] {
		return fmt.Errorf("'%s' 是保留词, 不能作账号名", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("非法账号名 '%s' (只能用 [A-Za-z0-9._-])", name)
	}
	return nil
}

// 目标账号必须落在 CMA_HOME 内 (护栏, 防路径穿越)
func mustSafe(dir string) {
	rel, err := filepath.Rel(cmaHome(), dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		die("拒绝操作 CMA_HOME 之外的目录 %s", dir)
	}
}

// ---------- keychain / email / flags ----------

func serviceName(configDir string) string {
	if configDir == "" {
		return "Claude Code-credentials"
	}
	sum := sha256.Sum256([]byte(configDir))
	return "Claude Code-credentials-" + fmt.Sprintf("%x", sum)[:8]
}

func keychainHas(service string) bool {
	return exec.Command("security", "find-generic-password", "-s", service, "-w").Run() == nil
}

// dir=账号目录(默认账号传 $HOME); configDir=传给 claude 的 CLAUDE_CONFIG_DIR(默认账号传 "")
func loggedIn(dir, configDir string) bool {
	if exists(filepath.Join(dir, ".credentials.json")) {
		return true
	}
	return keychainHas(serviceName(configDir))
}

func emailOf(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		return "-"
	}
	var d struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &d) != nil || d.OAuthAccount.EmailAddress == "" {
		return "-"
	}
	return d.OAuthAccount.EmailAddress
}

// 读 <dir>/.claude.json 里 claude 缓存的用量百分比 (5小时/7天 已用%)。
// 这是各账号上次运行 claude 时缓存的值, 非实时; 读不到返回 "-"。
func usageOf(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		return "-"
	}
	var d struct {
		Cached struct {
			Util struct {
				FiveHour struct {
					U *float64 `json:"utilization"`
				} `json:"five_hour"`
				SevenDay struct {
					U *float64 `json:"utilization"`
				} `json:"seven_day"`
			} `json:"utilization"`
		} `json:"cachedUsageUtilization"`
	}
	if json.Unmarshal(b, &d) != nil {
		return "-"
	}
	f, s := d.Cached.Util.FiveHour.U, d.Cached.Util.SevenDay.U
	if f == nil && s == nil {
		return "-"
	}
	part := func(p *float64) string {
		if p == nil {
			return "?"
		}
		return fmt.Sprintf("%.0f%%", *p)
	}
	return part(f) + "/" + part(s)
}

// flagDir=存放开关标记文件的目录 (默认账号传 CMA_HOME)
func flagFileSkip(flagDir string) string { return filepath.Join(flagDir, ".cma-flag-skip") }
func flagFileRC(flagDir string) string   { return filepath.Join(flagDir, ".cma-flag-rc") }

func flagsFor(flagDir string) []string {
	var f []string
	f = append(f, extraFlags()...)
	if exists(flagFileSkip(flagDir)) {
		f = append(f, "--dangerously-skip-permissions")
	}
	if exists(flagFileRC(flagDir)) {
		f = append(f, "--remote-control")
	}
	return f
}

func flagsLabel(flagDir string) string {
	var parts []string
	if exists(flagFileSkip(flagDir)) {
		parts = append(parts, "skip")
	}
	if exists(flagFileRC(flagDir)) {
		parts = append(parts, "rc")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "+")
}

func flagDirOf(name string) string {
	if name == "default" {
		return cmaHome()
	}
	return accountDir(name)
}

// ---------- 启动 (核心: 替换进程 exec claude) ----------

// configDir="" 表示默认账号(不设 CLAUDE_CONFIG_DIR); flagDir=开关标记所在目录
func execClaude(configDir, flagDir string, passArgs []string) {
	path, err := exec.LookPath("claude")
	if err != nil {
		die("找不到 claude 可执行文件: %v", err)
	}
	argv := []string{"claude"}
	argv = append(argv, flagsFor(flagDir)...)
	argv = append(argv, passArgs...)

	// 构造环境: 清掉第三方 provider 变量; 按需设/不设 CLAUDE_CONFIG_DIR
	var newEnv []string
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if providerVars[key] || key == "CLAUDE_CONFIG_DIR" {
			continue
		}
		newEnv = append(newEnv, e)
	}
	if configDir != "" {
		newEnv = append(newEnv, "CLAUDE_CONFIG_DIR="+configDir)
	}

	// syscall.Exec 替换当前进程 -> 交互式 claude 完全接管终端
	if err := syscall.Exec(path, argv, newEnv); err != nil {
		die("exec claude 失败: %v", err)
	}
}

// 回落默认账号(垫底): 不设 CLAUDE_CONFIG_DIR, 用默认账号的开关
func runDefault(args []string) { execClaude("", cmaHome(), args) }

// 启动指定账号
func launch(name string, args []string) {
	dir := accountDir(name)
	if name == "" || !isDir(dir) {
		fmt.Fprintf(os.Stderr, "cc2: 未知账号 '%s' —— 回落默认账号(垫底)\n", name)
		runDefault(args)
		return
	}
	execClaude(dir, dir, args)
}

// ---------- 子命令 ----------

func cmdAdd(args []string) {
	if len(args) == 0 {
		die("用法: cc2 add <账号名> [--global] [claude参数...]")
	}
	name := args[0]
	rest := args[1:]
	global := false
	if len(rest) > 0 && rest[0] == "--global" {
		global = true
		rest = rest[1:]
	}
	if err := validName(name); err != nil {
		die("%v", err)
	}
	dir := accountDir(name)
	if exists(dir) {
		die("账号 '%s' 已存在, 用 cc2 rm 先删或换名", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		die("无法创建 %s: %v", dir, err)
	}
	if global {
		fmt.Printf("▶ 账号 '%s' [全局]: 软链共享子项 -> %s (.claude.json/凭证仍独立)\n", name, globalDir())
		linkItems(dir)
	} else {
		fmt.Printf("▶ 账号 '%s' [独立]: %s\n", name, dir)
	}
	fmt.Printf("▶ 即将启动交互登录。若没自动弹出, 在会话里输入 /login。登录完成退出后即可 cc2 %s 启动。\n", name)
	execClaude(dir, dir, rest) // 新账号无开关 -> 纯净登录
}

func cmdRm(args []string) {
	if len(args) == 0 {
		die("用法: cc2 rm <账号名>")
	}
	name := args[0]
	dir := accountDir(name)
	mustSafe(dir)
	if !exists(dir) {
		die("账号 '%s' 不存在", name)
	}
	if isSymlink(dir) {
		os.Remove(dir) // 只删软链, 不碰目标
		fmt.Printf("已删除账号 '%s' (全局软链, 未触碰 %s)\n", name, globalDir())
	} else {
		os.RemoveAll(dir)
		fmt.Printf("已删除账号 '%s' (%s)\n", name, dir)
	}
	if bak := dir + ".isolated"; isDir(bak) {
		os.RemoveAll(bak)
		fmt.Printf("已清理备份 %s\n", bak)
	}
	fmt.Printf("提示: keychain 凭证条目未删, 如需彻底清理:\n  security delete-generic-password -s %q\n", serviceName(dir))
}

func cmdSet(args []string) {
	if len(args) < 3 {
		die("用法: cc2 set <账号|default> <skip|rc> <on|off>")
	}
	name, key, val := args[0], args[1], args[2]
	fd := flagDirOf(name)
	if name != "default" && !isDir(fd) {
		die("账号 '%s' 不存在", name)
	}
	var file string
	switch key {
	case "skip", "perm", "permissions":
		file = flagFileSkip(fd)
	case "rc", "remote", "remote-control":
		file = flagFileRC(fd)
	default:
		die("未知开关 '%s' (可用: skip / rc)", key)
	}
	switch val {
	case "on", "1", "true", "yes":
		if f, err := os.Create(file); err == nil {
			f.Close()
		}
		fmt.Printf("%s: %s = on\n", name, key)
	case "off", "0", "false", "no":
		os.Remove(file)
		fmt.Printf("%s: %s = off\n", name, key)
	default:
		die("值只能是 on / off")
	}
}

// ---------- link / unlink / ls ----------

// 在账号目录里, 把 globalLinks 里的"具体子项"逐个软链到全局。
// 账号里已有同名项先改名备份为 <item>.isolated。.claude.json/.credentials.json 永不软链。
func linkItems(dir string) {
	n := 0
	for _, item := range globalLinks() {
		if neverLink[item] {
			continue
		}
		src := filepath.Join(globalDir(), item)
		dst := filepath.Join(dir, item)
		if !exists(src) || isSymlink(dst) {
			continue
		}
		bak := dst + ".isolated"
		if exists(bak) {
			fmt.Printf("  跳过 %s (备份 %s.isolated 已存在, 请手动处理)\n", item, item)
			continue
		}
		if exists(dst) {
			if os.Rename(dst, bak) != nil {
				continue
			}
		}
		if os.Symlink(src, dst) == nil {
			n++
			fmt.Printf("  link: %s -> %s\n", item, src)
		}
	}
	fmt.Printf("  共链接 %d 项; .claude.json / .credentials.json 保持账号独立(不共享登录态)\n", n)
}

func cmdLink(args []string) {
	if len(args) == 0 {
		die("用法: cc2 link <账号名>")
	}
	name := args[0]
	dir := accountDir(name)
	if isSymlink(dir) {
		die("'%s' 是旧版整目录软链(有串号风险), 请先 cc2 unlink %s 修复再 link", name, name)
	}
	if !isDir(dir) {
		die("账号 '%s' 不存在, 先 cc2 add %s", name, name)
	}
	fmt.Printf("账号 '%s' -> [全局]\n", name)
	linkItems(dir)
}

func cmdUnlink(args []string) {
	if len(args) == 0 {
		die("用法: cc2 unlink <账号名>")
	}
	name := args[0]
	dir := accountDir(name)
	mustSafe(dir)
	// 兼容历史: 旧版整目录软链
	if isSymlink(dir) {
		bak := dir + ".isolated"
		os.Remove(dir)
		if isDir(bak) {
			os.Rename(bak, dir)
		} else {
			os.MkdirAll(dir, 0o755)
		}
		fmt.Printf("账号 '%s' 已从旧版整目录软链恢复[独立]\n", name)
		return
	}
	if !isDir(dir) {
		die("账号 '%s' 不存在", name)
	}
	n := 0
	for _, item := range globalLinks() {
		dst := filepath.Join(dir, item)
		if !isSymlink(dst) {
			continue
		}
		if target, err := os.Readlink(dst); err != nil || !strings.HasPrefix(target, globalDir()) {
			continue // 只解本工具建的全局软链
		}
		os.Remove(dst)
		if bak := dst + ".isolated"; exists(bak) {
			os.Rename(bak, dst)
		}
		n++
		fmt.Printf("  unlink: %s\n", item)
	}
	fmt.Printf("账号 '%s' -> [独立] (恢复 %d 项)\n", name, n)
}

func isGlobal(dir string) bool {
	if isSymlink(dir) {
		return true
	}
	for _, item := range globalLinks() {
		if isSymlink(filepath.Join(dir, item)) {
			return true
		}
	}
	return false
}

// 显示宽度 (东亚宽字符算 2), 用于中文列对齐
func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func isWide(r rune) bool {
	return (r >= 0x1100 && r <= 0x115F) ||
		(r >= 0x2E80 && r <= 0xA4CF) ||
		(r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6)
}

func pad(s string, n int) string {
	if d := n - dispWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// 列宽: 账号 模式 启动参数 已用 邮箱 (最后一列凭证不 pad)
var colW = []int{12, 8, 10, 12, 32}

func row(cols ...string) {
	var b strings.Builder
	b.WriteString("  ")
	for i, c := range cols {
		if i < len(colW) {
			b.WriteString(pad(c, colW[i]))
		} else {
			b.WriteString(c)
		}
	}
	fmt.Println(b.String())
}

func cmdLs() {
	os.MkdirAll(cmaHome(), 0o755)
	fmt.Printf("账号根目录: %s   (启动参数默认全关, 用 cc2 set 开关)\n", cmaHome())
	row("账号", "模式", "启动参数", "已用5h/7d", "登录邮箱", "凭证")
	fmt.Println("  " + strings.Repeat("-", 90))
	row("默认(垫底)", "-", flagsLabel(cmaHome()), usageOf(home()), emailOf(home()), "cc 垫底,永不修改")

	entries, _ := os.ReadDir(cmaHome())
	var names []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".isolated") {
			continue
		}
		if !isDir(accountDir(name)) { // 只列目录(含软链到目录)
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dir := accountDir(name)
		mode := "[独立]"
		if isGlobal(dir) {
			mode = "[全局]"
		}
		status := "· 未登录"
		if loggedIn(dir, dir) {
			status = "✓ 已登录"
		}
		row(name, mode, flagsLabel(dir), usageOf(dir), emailOf(dir), status)
	}
	if len(names) == 0 {
		fmt.Println("  (还没有账号, 用 cc2 add <名字> 添加)")
	}
	fmt.Println("  启动参数: skip=--dangerously-skip-permissions  rc=--remote-control")
	fmt.Println("  已用5h/7d: 各账号上次运行 claude 时缓存的用量百分比(非实时)")
}

// ---------- 轮询 ----------

func cmdNext(args []string) {
	os.MkdirAll(cmaHome(), 0o755)
	entries, _ := os.ReadDir(cmaHome())
	var names []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".isolated") || !isDir(accountDir(name)) {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "cc2: 还没有任何账号 —— 回落默认账号(垫底)")
		runDefault(args)
		return
	}
	sort.Strings(names)
	rot := filepath.Join(cmaHome(), ".rotation")
	cursor := 0
	if b, err := os.ReadFile(rot); err == nil {
		fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &cursor)
	}
	if cursor < 0 {
		cursor = 0
	}
	idx := cursor % len(names)
	pick := names[idx]
	os.WriteFile(rot, fmt.Appendf(nil, "%d", (idx+1)%len(names)), 0o644)
	fmt.Printf("▶ 轮询选中账号: %s  (第 %d/%d 个)\n", pick, idx+1, len(names))
	launch(pick, args)
}

// ---------- help / main ----------

func cmdHelp() {
	fmt.Print(`cc2 — 官网多账号并行 / 轮询 (默认账号永远垫底, 不会被本工具修改)

  cc2 add <名字> [--global]  新增账号并交互登录; --global 直接共享全局设置
  cc2 <名字> [参数...]        以该账号启动 claude (参数原样透传, 如 --resume)
  cc2 next [参数...]          轮询: 自动挑下一个账号启动, 均摊用量
  cc2 link <名字>            切到[全局]: 软链 skills/plugins/settings 等子项到 ~/.claude
  cc2 unlink <名字>          切回[独立]: 删除这些软链, 从备份恢复
  cc2 set <名字|default> skip|rc on|off   开关启动参数(默认全关):
                              skip=--dangerously-skip-permissions  rc=--remote-control
  cc2 ls                    列出账号 / 模式 / 启动参数 / 登录邮箱 / 凭证状态
  cc2 rm <名字>             删除某账号目录 (从不碰 ~/.claude)
  cc2 help                  本帮助

说明:
  * 每个账号是一个 CLAUDE_CONFIG_DIR 目录, 凭证由 claude 按目录路径 hash 隔离,
    因此不同账号可在多个终端"同时并行跑", 各自消耗各自用量。
  * [全局]模式只软链无身份的子项; .claude.json(登录态)/.credentials.json(凭证)
    永不软链 —— 各账号登录态完全独立, 绝不串号。
  * 现有的 cc / ccr / ccl 完全不受影响; 任何解析失败一律回落默认账号。
`)
}

func main() {
	args := os.Args[1:]
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	rest := []string{}
	if len(args) > 1 {
		rest = args[1:]
	}
	switch verb {
	case "add":
		cmdAdd(rest)
	case "rm":
		cmdRm(rest)
	case "ls", "list":
		cmdLs()
	case "next":
		cmdNext(rest)
	case "link":
		cmdLink(rest)
	case "unlink":
		cmdUnlink(rest)
	case "set":
		cmdSet(rest)
	case "help", "-h", "--help", "":
		cmdHelp()
	default:
		launch(verb, rest)
	}
}
