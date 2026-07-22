package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func watchPidFile() string { return filepath.Join(cmaHome(), ".watch.pid") }

func readWatchPid() int {
	b, err := os.ReadFile(watchPidFile())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// 进程是否存活 (signal 0 探测)
func processAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func watchStop() {
	pid := readWatchPid()
	if !processAlive(pid) {
		os.Remove(watchPidFile())
		fmt.Println(L("no running cc2 watch found.", "没有正在运行的 cc2 watch。"))
		return
	}
	syscall.Kill(pid, syscall.SIGTERM)
	os.Remove(watchPidFile())
	fmt.Printf(L("stopped cc2 watch (pid %d).\n", "已停止 cc2 watch (pid %d)。\n"), pid)
}

func watchStatus() {
	pid := readWatchPid()
	if processAlive(pid) {
		fmt.Printf(L("cc2 watch is running (pid %d).\n", "cc2 watch 正在运行 (pid %d)。\n"), pid)
	} else {
		fmt.Println(L("cc2 watch is not running.", "cc2 watch 未运行。"))
	}
}

// 默认账号(垫底)是否有活跃 session (存在无 CLAUDE_CONFIG_DIR 的 claude 主进程)
func defaultHasActiveSession() bool {
	for _, pid := range claudePids() {
		cfg, _, _ := procInfo(pid)
		if cfg == "" {
			return true
		}
	}
	return false
}

// 选下一个可切换账号: 库中排除当前 active, 找 five_hour 用量 < 阈值 的第一个
func pickNextAccount(threshold float64) (string, float64) {
	active := readActive()
	names := listAccounts()
	// 从 active 的下一个开始轮转, 保证顺序均摊
	start := 0
	for i, n := range names {
		if n == active {
			start = i + 1
			break
		}
	}
	ordered := append(append([]string{}, names[start:]...), names[:start]...)
	for _, n := range ordered {
		if n == active {
			continue
		}
		dir := accountDir(n)
		pct, ok := usageFiveHour(dir)
		if !ok {
			continue // 查不到(未登录/过期)的跳过
		}
		if pct < threshold {
			return n, pct
		}
	}
	return "", 0
}

// 桌面通知 (best-effort)
func notify(title, msg string) {
	exec.Command("terminal-notifier", "-title", title, "-message", msg).Run()
}

func ts() string { return time.Now().Format("15:04:05") }

// cc2 watch [间隔秒] [阈值%]: 常驻监测默认(垫底)账号, 逼近阈值自动 use 下一个账号
func cmdWatch(args []string) {
	// 子命令: stop / status
	if len(args) > 0 {
		switch args[0] {
		case "stop", "off":
			watchStop()
			return
		case "status":
			watchStatus()
			return
		}
	}
	// 防重复启动
	if pid := readWatchPid(); processAlive(pid) {
		fmt.Printf(L("cc2 watch already running (pid %d); stop it with: cc2 watch stop\n",
			"cc2 watch 已在运行 (pid %d); 用 cc2 watch stop 停止\n"), pid)
		return
	}
	os.MkdirAll(cmaHome(), 0o755)
	os.WriteFile(watchPidFile(), fmt.Appendf(nil, "%d", os.Getpid()), 0o644)
	defer os.Remove(watchPidFile())

	interval := 60 * time.Second
	threshold := 95.0
	var nums []string
	for _, a := range args {
		nums = append(nums, a)
	}
	if len(nums) >= 1 {
		if v, err := strconv.Atoi(strings.TrimSuffix(nums[0], "s")); err == nil && v > 0 {
			interval = time.Duration(v) * time.Second
		}
	}
	if len(nums) >= 2 {
		if v, err := strconv.ParseFloat(strings.TrimSuffix(nums[1], "%"), 64); err == nil && v > 0 {
			threshold = v
		}
	}
	const minInterval = 15 * time.Second
	if interval < minInterval {
		fmt.Printf(L("[%s] interval too short (usage API rate-limit risk), raised to %s\n",
			"[%s] 间隔过短(易触发用量 API 限流), 已调整为 %s\n"), ts(), minInterval)
		interval = minInterval
	}

	fmt.Printf(L("[%s] cc2 watch started: watching default account, every %s, threshold %.0f%%\n",
		"[%s] cc2 watch 启动: 监测默认(垫底)账号, 每 %s 检查一次, 阈值 %.0f%%\n"), ts(), interval, threshold)
	fmt.Printf(L("[%s] queries usage only when the default account has an active session; auto cc2 use next account near threshold\n",
		"[%s] 仅在默认账号有活跃 session 时查询用量; 逼近阈值自动 cc2 use 下一个账号\n"), ts())
	fmt.Printf(L("[%s] current default account: %s <%s>  (Ctrl-C to quit)\n",
		"[%s] 当前默认账号: %s <%s>  (Ctrl-C 退出)\n"), ts(), readActive(), emailOf(home()))

	// 优雅退出
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	watchOnce(threshold) // 立即先跑一次
	for {
		select {
		case <-stop:
			fmt.Printf(L("\n[%s] cc2 watch stopped.\n", "\n[%s] cc2 watch 已停止。\n"), ts())
			return
		case <-tick.C:
			watchOnce(threshold)
		}
	}
}

func watchOnce(threshold float64) {
	if !defaultHasActiveSession() {
		return // 无活跃 session, 不查(省 API)
	}
	pct, ok := usageFiveHour("")
	if !ok {
		fmt.Printf(L("[%s] default account has an active session, but usage query failed (token expired?)\n",
			"[%s] 默认账号有活跃 session, 但用量查询失败(token 过期?)\n"), ts())
		return
	}
	fmt.Printf(L("[%s] default account five_hour used %.0f%%\n", "[%s] 默认账号 five_hour 已用 %.0f%%\n"), ts(), pct)
	if pct < threshold {
		return
	}
	next, npct := pickNextAccount(threshold)
	if next == "" {
		fmt.Printf(L("[%s] ⚠️ default account hit %.0f%%, but no account to switch to (others near limit or not logged in)\n",
			"[%s] ⚠️ 默认账号已达 %.0f%%, 但没有可切换的账号(其余都逼近上限或未登录)\n"), ts(), pct)
		notify("cc2 watch", fmt.Sprintf(L("default usage %.0f%%, no account to switch to!", "默认账号用量 %.0f%%, 无可切换账号!"), pct))
		return
	}
	if err := doUse(next, false); err != nil {
		fmt.Printf(L("[%s] ❌ auto-switch to '%s' failed: %v\n", "[%s] ❌ 自动切换到 '%s' 失败: %v\n"), ts(), next, err)
		return
	}
	msg := fmt.Sprintf(L("default usage %.0f%% -> auto-switched to '%s' (%.0f%%)",
		"默认账号用量 %.0f%% -> 已自动切换为 '%s' (%.0f%%)"), pct, next, npct)
	fmt.Printf("[%s] ✅ %s\n", ts(), msg)
	fmt.Printf(L("[%s]    note: running sessions are unaffected (keep old creds); newly launched default claude uses the new account; cc2 restore to undo\n",
		"[%s]    注: 运行中的 session 不受影响(仍用旧凭证), 新开的默认 claude 才用新账号; cc2 restore 可还原\n"), ts())
	notify(L("cc2 watch auto-switch", "cc2 watch 自动切换"), msg)
}
