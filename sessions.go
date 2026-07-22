package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 一个活跃 claude session (= 一个 claude 主进程)
type sess struct {
	pid     string
	account string // 账号显示名
	email   string
	project string // PWD
	title   string // 最近的用户请求(近似标题)
	last    time.Time
	busy    bool
	etime   string
}

// 枚举正在运行的 claude 主进程 pid
func claudePids() []string {
	out, err := exec.Command("pgrep", "-x", "claude").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// 从 `ps eww` 的 command+env 串里取某环境变量值 (路径值无空格, 够用)
func envVal(s, key string) string {
	i := strings.Index(s, " "+key+"=")
	if i < 0 {
		if strings.HasPrefix(s, key+"=") {
			i = -1
		} else {
			return ""
		}
	}
	rest := s[i+len(key)+2:] // 跳过 " key="
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return rest[:j]
	}
	return rest
}

// 读进程的 CLAUDE_CONFIG_DIR / PWD / etime
func procInfo(pid string) (configDir, pwd, etime string) {
	out, _ := exec.Command("ps", "eww", "-p", pid, "-o", "command=").Output()
	s := string(out)
	configDir = envVal(s, "CLAUDE_CONFIG_DIR")
	pwd = envVal(s, "PWD")
	eo, _ := exec.Command("ps", "-p", pid, "-o", "etime=").Output()
	etime = strings.TrimSpace(string(eo))
	return
}

// PWD -> ~/.claude/projects 下的编码目录名 (claude 规则: / 和 . 均替换为 -)
func projectDir(pwd string) string {
	enc := strings.NewReplacer("/", "-", ".", "-").Replace(pwd)
	return filepath.Join(globalDir(), "projects", enc)
}

// 取该项目最近活跃 session 的标题(首条 user 消息)与最近活动时间(文件 mtime)
func sessionMeta(pwd string) (title string, last time.Time) {
	dir := projectDir(pwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}
	// 选 mtime 最新的 .jsonl
	var newest string
	var newestT time.Time
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().After(newestT) {
			newestT = fi.ModTime()
			newest = filepath.Join(dir, e.Name())
		}
	}
	if newest == "" {
		return "", time.Time{}
	}
	return firstUserText(newest), newestT
}

// 读 jsonl 前若干行, 找首条 user 消息的文本
func firstUserText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	n := 0
	for sc.Scan() && n < 400 {
		n++
		var r struct {
			Type    string `json:"type"`
			IsMeta  bool   `json:"isMeta"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Type != "user" || r.IsMeta {
			continue
		}
		var text string
		var s string
		if json.Unmarshal(r.Message.Content, &s) == nil && s != "" {
			text = s
		} else {
			var arr []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(r.Message.Content, &arr) == nil {
				for _, p := range arr {
					if p.Text != "" {
						text = p.Text
						break
					}
				}
			}
		}
		if text == "" || isSystemish(text) {
			continue
		}
		return oneLine(text)
	}
	return ""
}

// 跳过 claude 注入的系统/命令消息, 只留真正的用户请求
func isSystemish(s string) bool {
	t := strings.TrimSpace(s)
	for _, p := range []string{"<local-command", "<command-name", "<command-message",
		"Caveat:", "<system-reminder", "<user-prompt-submit", "[Request interrupted"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	r := []rune(s)
	if len(r) > 32 {
		return string(r[:32]) + "…"
	}
	return s
}

// 账号显示名 + 邮箱 (configDir 空 = 默认垫底)
func accountOf(configDir string) (name, email string) {
	if configDir == "" {
		return "默认(垫底)", emailOf(home())
	}
	return filepath.Base(configDir), emailOf(configDir)
}

func gatherSessions() []sess {
	pids := claudePids()
	var out []sess
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, pid := range pids {
		wg.Add(1)
		go func(pid string) {
			defer wg.Done()
			cfg, pwd, et := procInfo(pid)
			name, email := accountOf(cfg)
			title, last := sessionMeta(pwd)
			busy := !last.IsZero() && time.Since(last) < 90*time.Second
			mu.Lock()
			out = append(out, sess{pid, name, email, pwd, title, last, busy, et})
			mu.Unlock()
		}(pid)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].account != out[j].account {
			return out[i].account < out[j].account
		}
		return out[i].project < out[j].project
	})
	return out
}

func cmdSessions() {
	ss := gatherSessions()
	if len(ss) == 0 {
		fmt.Println("当前没有正在使用的 claude session。")
		return
	}
	fmt.Printf("正在使用的 claude session (%d 个):\n", len(ss))
	pf := "  %s%s%s%s%s\n"
	fmt.Printf(pf, pad("状态", 6), pad("账号", 14), pad("登录邮箱", 30), pad("项目", 26), "最近请求 / 活动")
	fmt.Println("  " + strings.Repeat("-", 108))
	for _, s := range ss {
		st := "○闲"
		if s.busy {
			st = "●忙"
		}
		act := "—"
		if !s.last.IsZero() {
			act = agoStr(time.Since(s.last))
		}
		title := s.title
		if title == "" {
			title = "(无标题)"
		}
		proj := s.project
		if proj == "" {
			proj = "-"
		} else {
			proj = filepath.Base(proj)
		}
		fmt.Printf(pf, pad(st, 6), pad(s.account, 14), pad(s.email, 30), pad(proj, 26),
			title+"  ["+act+"]")
	}
	fmt.Println("  ●忙=90秒内有活动(近似)  ○闲=空闲等待; 账号为默认(垫底)表示未设 CLAUDE_CONFIG_DIR")
}

func agoStr(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d秒前", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d分前", int(d.Minutes()))
	}
	return fmt.Sprintf("%d时前", int(d.Hours()))
}
