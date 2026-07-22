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
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 版本号 (由 -ldflags "-X main.version=..." 注入; 默认 dev)
var version = "dev"

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
	"use": true, "restore": true, "sessions": true, "ps": true,
	"watch": true, "version": true,
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

type utilField struct {
	U *float64 `json:"utilization"`
}

func pctOrDash(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

func nowMs() int64 { return time.Now().UnixMilli() }

// 读该 configDir 账号在 keychain 里的 oauth access token 与过期时间(ms)。
// configDir="" 表示默认账号 (服务名无后缀)。
func oauthToken(configDir string) (token string, expiresAt int64) {
	out, err := exec.Command("security", "find-generic-password", "-s", serviceName(configDir), "-w").Output()
	if err != nil {
		return "", 0
	}
	var c struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(out, &c) != nil {
		return "", 0
	}
	return c.OAuth.AccessToken, c.OAuth.ExpiresAt
}

func osUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "claude-code-user"
}

// 读某 keychain 服务名的原始值 (去尾换行)
func keychainRead(service string) ([]byte, bool) {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return nil, false
	}
	return bytes.TrimRight(out, "\n"), true
}

// 写某 keychain 服务名 (-U 存在则更新)
func keychainWrite(service string, data []byte) error {
	return exec.Command("security", "add-generic-password",
		"-a", osUser(), "-s", service, "-w", string(data), "-U").Run()
}

func defaultClaudeJSON() string { return filepath.Join(home(), ".claude.json") }
func activePath() string        { return filepath.Join(cmaHome(), ".active") }
func slotBackupDir() string     { return filepath.Join(cmaHome(), ".slot-backup") }

func readActive() string {
	b, _ := os.ReadFile(activePath())
	return strings.TrimSpace(string(b))
}
func writeActive(name string) { os.WriteFile(activePath(), []byte(name), 0o644) }

func copyFile(src, dst string, perm os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, perm)
}

// 只把 src(.claude.json) 里的"身份字段"合并进 dst, 保留 dst 其余本机状态
func mergeIdentity(src, dst string) error {
	sb, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	var sm, dm map[string]json.RawMessage
	if json.Unmarshal(sb, &sm) != nil {
		return fmt.Errorf("账号 .claude.json 解析失败")
	}
	if db, e := os.ReadFile(dst); e == nil {
		json.Unmarshal(db, &dm)
	}
	if dm == nil {
		dm = map[string]json.RawMessage{}
	}
	for _, k := range []string{"oauthAccount", "cachedUsageUtilization"} {
		if v, ok := sm[k]; ok {
			dm[k] = v
		} else {
			delete(dm, k)
		}
	}
	out, err := json.MarshalIndent(dm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dst, out, 0o600)
}

// 实时查询原始返回: GET /api/oauth/usage (纯查询, 不消耗对话额度; 不刷新/不写 keychain)。
// token 缺失或已过期或请求失败 -> ok=false。
func fetchUsageRaw(configDir string) ([]byte, bool) {
	tok, exp := oauthToken(configDir)
	if tok == "" || (exp > 0 && exp <= nowMs()) {
		return nil, false
	}
	req, err := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, false
	}
	body, _ := io.ReadAll(resp.Body)
	return body, true
}

// 实时用量显示串 "5h/7d"
func usageLive(configDir string) (string, bool) {
	body, ok := fetchUsageRaw(configDir)
	if !ok {
		return "", false
	}
	var d struct {
		FiveHour *utilField `json:"five_hour"`
		SevenDay *utilField `json:"seven_day"`
	}
	if json.Unmarshal(body, &d) != nil {
		return "", false
	}
	f := func(u *utilField) string {
		if u == nil {
			return "-"
		}
		return pctOrDash(u.U)
	}
	return f(d.FiveHour) + "/" + f(d.SevenDay), true
}

// 实时 five_hour 已用百分比 (数值), 供 watch 判断阈值
func usageFiveHour(configDir string) (float64, bool) {
	body, ok := fetchUsageRaw(configDir)
	if !ok {
		return 0, false
	}
	var d struct {
		FiveHour *utilField `json:"five_hour"`
	}
	if json.Unmarshal(body, &d) != nil || d.FiveHour == nil || d.FiveHour.U == nil {
		return 0, false
	}
	return *d.FiveHour.U, true
}

// 回退: 读 <dir>/.claude.json 里 claude 上次缓存的用量 (非实时)。
func usageCached(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		return "-"
	}
	var d struct {
		Cached struct {
			Util struct {
				FiveHour utilField `json:"five_hour"`
				SevenDay utilField `json:"seven_day"`
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
	return pctOrDash(f) + "/" + pctOrDash(s)
}

// 优先实时查询, 失败回退缓存。返回 (显示串, 是否实时)。
func usageOf(dir, configDir string) (string, bool) {
	if s, ok := usageLive(configDir); ok {
		return s, true
	}
	return usageCached(dir), false
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

func cmdLs(args []string) {
	os.MkdirAll(cmaHome(), 0o755)
	all := false
	for _, a := range args {
		if a == "-a" || a == "--all" {
			all = true
		}
	}
	names := listAccountsAll(all)

	// 并发预取用量: key=dir, 结果含 显示串 + 是否实时。默认账号 key=home。
	type ures struct {
		s    string
		live bool
	}
	jobs := map[string]string{home(): ""} // dir -> configDir
	for _, n := range names {
		jobs[accountDir(n)] = accountDir(n)
	}
	usage := map[string]ures{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for dir, cfg := range jobs {
		wg.Add(1)
		go func(dir, cfg string) {
			defer wg.Done()
			s, live := usageOf(dir, cfg)
			mu.Lock()
			usage[dir] = ures{s, live}
			mu.Unlock()
		}(dir, cfg)
	}
	wg.Wait()
	// 缓存值(非实时)加 ~ 前缀以示区分
	label := func(dir string) string {
		u := usage[dir]
		if !u.live && u.s != "-" {
			return "~" + u.s
		}
		return u.s
	}

	fmt.Printf("账号根目录: %s   (启动参数默认全关, 用 cc2 set 开关)\n", cmaHome())
	row("账号", "模式", "启动参数", "已用5h/7d", "登录邮箱", "凭证")
	fmt.Println("  " + strings.Repeat("-", 90))
	row("默认(垫底)", "-", flagsLabel(cmaHome()), label(home()), emailOf(home()), "cc 垫底,永不修改")
	active := readActive()
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
		disp := name
		if name == active {
			disp = name + " ★"
		}
		row(disp, mode, flagsLabel(dir), label(dir), emailOf(dir), status)
	}
	if len(names) == 0 {
		fmt.Println("  (还没有账号, 用 cc2 add <名字> 添加)")
	}
	fmt.Println("  启动参数: skip=--dangerously-skip-permissions  rc=--remote-control")
	fmt.Println("  已用5h/7d: 实时查询各账号剩余额度(5小时/7天已用%); ~前缀=token过期回退的缓存值")
	fmt.Println("  ★ = cc2 use 设为默认(cc)槽位的账号; cc2 use <账号> 切换, cc2 restore 还原")
}

// 列出 CMA_HOME 下的账号名(默认跳过 . 开头的内部目录和 .isolated 备份)
func listAccounts() []string { return listAccountsAll(false) }

func listAccountsAll(all bool) []string {
	entries, _ := os.ReadDir(cmaHome())
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !isDir(accountDir(name)) {
			continue
		}
		// 内部目录: .slot-backup / .isolated 备份等 (账号名不会以 . 开头)
		if !all && (strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".isolated")) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ---------- 轮询 ----------

func cmdNext(args []string) {
	os.MkdirAll(cmaHome(), 0o755)
	names := listAccounts()
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "cc2: 还没有任何账号 —— 回落默认账号(垫底)")
		runDefault(args)
		return
	}
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

// 备份当前默认槽位 (keychain + ~/.claude.json + 来源账号), 供 cc2 restore 还原
func backupDefaultSlot() {
	bdir := slotBackupDir()
	os.MkdirAll(bdir, 0o700)
	if cred, ok := keychainRead(serviceName("")); ok {
		os.WriteFile(filepath.Join(bdir, "credentials.json"), cred, 0o600)
	}
	if b, err := os.ReadFile(defaultClaudeJSON()); err == nil {
		os.WriteFile(filepath.Join(bdir, "claude.json"), b, 0o600)
	}
	os.WriteFile(filepath.Join(bdir, "from"), []byte(readActive()), 0o644)
}

// cc2 use <账号> [--full]: 把账号凭证覆盖到默认全局环境(cc 用的槽位)
// 默认只换登录身份(oauthAccount); --full 整体覆盖 .claude.json。覆盖前自动备份。
func cmdUse(args []string) {
	full, name := false, ""
	for _, a := range args {
		if a == "--full" {
			full = true
		} else if name == "" {
			name = a
		}
	}
	if name == "" {
		die("用法: cc2 use <账号> [--full]  (默认只换登录身份; --full 整体覆盖 .claude.json)")
	}
	if err := doUse(name, full); err != nil {
		die("%v", err)
	}
	mode := "只换登录身份"
	if full {
		mode = "整体覆盖 .claude.json"
	}
	fmt.Printf("✅ 默认账号(cc)已切换为 '%s' <%s> [%s]\n", name, emailOf(accountDir(name)), mode)
	fmt.Println("   不带参数的 cc/默认 claude 现在用该账号; cc2 restore 可还原上一个默认。")
}

// use 的核心: 备份默认槽位 -> 覆盖凭证 -> 覆盖/合并 .claude.json -> 记 active。
// 供 cmdUse 与 watch 复用。
func doUse(name string, full bool) error {
	dir := accountDir(name)
	if !isDir(dir) {
		return fmt.Errorf("账号 '%s' 不存在", name)
	}
	xcred, ok := keychainRead(serviceName(dir))
	if !ok {
		return fmt.Errorf("账号 '%s' 没有 keychain 凭证, 先 cc2 %s 登录", name, name)
	}
	backupDefaultSlot() // 打破"垫底不可改", 先留后路
	if err := keychainWrite(serviceName(""), xcred); err != nil {
		return fmt.Errorf("写默认 keychain 失败: %v", err)
	}
	xjson := filepath.Join(dir, ".claude.json")
	if full {
		if err := copyFile(xjson, defaultClaudeJSON(), 0o600); err != nil {
			return fmt.Errorf("覆盖 ~/.claude.json 失败: %v", err)
		}
	} else {
		if err := mergeIdentity(xjson, defaultClaudeJSON()); err != nil {
			return fmt.Errorf("合并登录身份失败: %v", err)
		}
	}
	writeActive(name)
	return nil
}

// cc2 restore: 从最近一次 use 前的备份还原默认槽位
func cmdRestore() {
	bdir := slotBackupDir()
	cred, err := os.ReadFile(filepath.Join(bdir, "credentials.json"))
	if err != nil {
		die("没有可恢复的备份 (还没执行过 cc2 use)")
	}
	if err := keychainWrite(serviceName(""), bytes.TrimRight(cred, "\n")); err != nil {
		die("还原默认 keychain 失败: %v", err)
	}
	if b, e := os.ReadFile(filepath.Join(bdir, "claude.json")); e == nil {
		os.WriteFile(defaultClaudeJSON(), b, 0o600)
	}
	from, _ := os.ReadFile(filepath.Join(bdir, "from"))
	writeActive(strings.TrimSpace(string(from)))
	fmt.Println("✅ 已从备份还原默认账号槽位。")
}

// 首次引导: 账号库为空且默认账号已登录时, 把默认账号存档为账号'1'(全局模式)
func maybeInit() {
	if os.Getenv("CMA_NO_INIT") != "" || len(listAccounts()) > 0 {
		return
	}
	cred, ok := keychainRead(serviceName(""))
	if !ok {
		return // 默认账号还没登录, 无可存档
	}
	dir := accountDir("1")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	keychainWrite(serviceName(dir), cred)
	if b, err := os.ReadFile(defaultClaudeJSON()); err == nil {
		os.WriteFile(filepath.Join(dir, ".claude.json"), b, 0o600)
	}
	fmt.Fprintln(os.Stderr, "cc2: 首次初始化 —— 已把默认账号存档为账号 '1' (全局模式)")
	linkItems(dir)
	writeActive("1")
}

func cmdHelp() {
	fmt.Print(`cc2 — 官网多账号并行 / 轮询 (默认账号永远垫底, 不会被本工具修改)

  cc2 add <名字> [--global]  新增账号并交互登录; --global 直接共享全局设置
  cc2 <名字> [参数...]        以该账号启动 claude (参数原样透传, 如 --resume)
  cc2 next [参数...]          轮询: 自动挑下一个账号启动, 均摊用量
  cc2 link <名字>            切到[全局]: 软链 skills/plugins/settings 等子项到 ~/.claude
  cc2 unlink <名字>          切回[独立]: 删除这些软链, 从备份恢复
  cc2 set <名字|default> skip|rc on|off   开关启动参数(默认全关):
                              skip=--dangerously-skip-permissions  rc=--remote-control
  cc2 ls [-a]               列出账号 / 模式 / 启动参数 / 用量 / 邮箱 / 凭证 (-a 含内部目录)
  cc2 rm <名字>             删除某账号目录 (从不碰 ~/.claude)
  cc2 use <名字> [--full]    把该账号凭证覆盖到默认环境(cc 用的); 覆盖前自动备份
                              默认只换登录身份; --full 整体覆盖 .claude.json
  cc2 restore               还原 cc2 use 前备份的默认账号
  cc2 sessions              列出所有正在使用的 claude session (账号/项目/标题/忙闲)
  cc2 watch [间隔s] [阈值%]  常驻监测默认账号, 逼近阈值(默认95%)自动切下一个账号
  cc2 version               显示版本
  cc2 help                  本帮助

首次安装且账号库为空时, 会自动把默认账号存档为账号 '1'(全局模式)。
(设 CMA_NO_INIT=1 可跳过该引导)

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
	// version/help 不触发初始化; 其余命令先做首次引导(仅库空时动作)
	switch verb {
	case "version", "-v", "--version", "help", "-h", "--help", "":
	default:
		maybeInit()
	}
	switch verb {
	case "add":
		cmdAdd(rest)
	case "rm":
		cmdRm(rest)
	case "ls", "list":
		cmdLs(rest)
	case "next":
		cmdNext(rest)
	case "link":
		cmdLink(rest)
	case "unlink":
		cmdUnlink(rest)
	case "set":
		cmdSet(rest)
	case "use":
		cmdUse(rest)
	case "restore":
		cmdRestore()
	case "sessions", "ps":
		cmdSessions()
	case "watch":
		cmdWatch(rest)
	case "version", "-v", "--version":
		fmt.Println("cc2 " + version)
	case "help", "-h", "--help", "":
		cmdHelp()
	default:
		if strings.HasPrefix(verb, "-") {
			fmt.Fprintf(os.Stderr, "cc2: 未知选项 %q (账号名不能以 - 开头; 参数请放在账号名之后)\n\n", verb)
			cmdHelp()
			os.Exit(1)
		}
		launch(verb, rest)
	}
}
