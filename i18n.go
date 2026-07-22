package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 当前语言: "en"(默认) 或 "zh"。由 loadLang 从 CMA_LANG 或 ~/.cc2/.lang 载入。
var lang = "en"

func langPath() string { return filepath.Join(cmaHome(), ".lang") }

func normLang(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "zh", "chinese", "cn", "zh-cn", "中文":
		return "zh"
	case "en", "english":
		return "en"
	}
	return ""
}

// 启动时载入语言偏好 (环境变量优先于 .lang 文件)
func loadLang() {
	if v := normLang(os.Getenv("CMA_LANG")); v != "" {
		lang = v
		return
	}
	if b, err := os.ReadFile(langPath()); err == nil {
		if l := normLang(string(b)); l != "" {
			lang = l
		}
	}
}

// L: 就地双语文案。默认(en)返回第一个参数, 中文返回第二个。
func L(en, zh string) string {
	if lang == "zh" {
		return zh
	}
	return en
}

// cc2 setlanguage <english|chinese>
func cmdSetLanguage(args []string) {
	if len(args) == 0 {
		die(L("usage: cc2 setlanguage <english|chinese>", "用法: cc2 setlanguage <english|chinese>"))
	}
	l := normLang(args[0])
	if l == "" {
		die(L("unknown language %q (use: english / chinese)", "未知语言 %q (可用: english / chinese)"), args[0])
	}
	os.MkdirAll(cmaHome(), 0o755)
	if err := os.WriteFile(langPath(), []byte(l), 0o644); err != nil {
		die("%v", err)
	}
	lang = l
	if l == "zh" {
		fmt.Println("语言已设为中文。")
	} else {
		fmt.Println("Language set to English.")
	}
}
