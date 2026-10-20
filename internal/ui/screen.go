// Package ui 负责用户在屏幕上看到的一切：分步骤显示、进度、警告、提问和结果摘要。
//
// 每个步骤在屏幕上只占一行：进行中时由进度原地刷新这一行，完成后换成
// “✓ 结果 · 耗时”。警告单独占一行并汇总到最终结果；细节只写日志
// （--verbose 时也显示在屏幕上）。
package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Logger 接收写进日志文件的内容。
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Failure 是本次任务的失败原因和建议；只保留第一条，它通常就是根因。
type Failure struct {
	Reason string
	Hint   string
}

// Timing 记录每个步骤的耗时，显示在结果摘要里。
type Timing struct {
	Title   string
	Elapsed time.Duration
}

type stage struct {
	num, total int
	title      string
	started    time.Time
}

// Screen 把输出写到终端（通常是标准错误）。
type Screen struct {
	mu       sync.Mutex
	out      io.Writer
	tty      bool
	verbose  bool
	log      Logger
	now      func() time.Time
	cur      *stage
	failed   string
	warnings []string
	timings  []Timing
	failure  *Failure
	// lineBusy 表示当前行正被步骤标题或进度占用，输出别的内容前要先清掉。
	lineBusy bool
	lastNote time.Time
}

// New 创建屏幕输出。tty 为 true 时使用颜色并原地刷新进度。
func New(out io.Writer, tty bool, log Logger) *Screen {
	if log == nil {
		log = nopLogger{}
	}
	return &Screen{out: out, tty: tty, log: log, now: time.Now}
}

// SetVerbose 打开后，细节也会显示在屏幕上。
func (s *Screen) SetVerbose(v bool) { s.verbose = v }

// IsTTY 表示输出是否是交互终端。
func (s *Screen) IsTTY() bool { return s.tty }

func (s *Screen) color(code, text string) string {
	if !s.tty {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

// clearLine 清掉被步骤标题或进度占用的当前行。调用方需持有锁。
func (s *Screen) clearLine() {
	if s.tty && s.lineBusy {
		fmt.Fprint(s.out, "\r\033[K")
	}
	s.lineBusy = false
}

// draw 在当前行原地画出一行内容（只用于终端）。调用方需持有锁。
func (s *Screen) draw(line string) {
	// 一次写入覆盖旧内容，再清除右侧残留；先写一整行空格会在慢终端上闪烁。
	fmt.Fprint(s.out, "\r"+line+"\033[K")
	s.lineBusy = true
}

// prefix 是当前步骤的前缀，例如“[5/7] 保存镜像”。调用方需持有锁。
func (s *Screen) prefix() string {
	if s.cur == nil {
		return ""
	}
	return fmt.Sprintf("[%d/%d] %s", s.cur.num, s.cur.total, s.cur.title)
}

// label 是进度行开头：步骤前缀加上正在处理的对象；子步骤与步骤同名时不重复。
func (s *Screen) label(item string) string {
	p := s.prefix()
	if s.cur != nil && item == s.cur.title {
		item = ""
	}
	switch {
	case p != "" && item != "":
		return p + " · " + item
	default:
		return p + item
	}
}

// Begin 开始一个步骤。
func (s *Screen) Begin(num, total int, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = &stage{num: num, total: total, title: title, started: s.now()}
	s.log.Printf("===== %s =====", s.prefix())
	if s.tty {
		s.draw(s.prefix() + " …")
	} else {
		fmt.Fprintln(s.out, s.prefix())
	}
}

// Hold 在步骤中需要列菜单或提问时，先把步骤标题固定成完整的一行。
func (s *Screen) Hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tty || s.cur == nil {
		return
	}
	s.draw(s.prefix())
	fmt.Fprintln(s.out)
	s.lineBusy = false
}

// End 结束当前步骤，显示“✓ 结果 · 耗时”。
func (s *Screen) End(summary string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return
	}
	elapsed := s.now().Sub(s.cur.started)
	s.timings = append(s.timings, Timing{Title: s.cur.title, Elapsed: elapsed})
	tail := Elapsed(elapsed)
	if summary != "" {
		tail = summary + " · " + tail
	}
	s.log.Printf("完成：%s · %s", s.prefix(), tail)
	s.clearLine()
	fmt.Fprintf(s.out, "%s %s %s\n", s.prefix(), s.color("1;32", "✓"), tail)
	s.cur = nil
}

// Fail 把当前步骤标为失败。
func (s *Screen) Fail() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return
	}
	s.timings = append(s.timings, Timing{Title: s.cur.title, Elapsed: s.now().Sub(s.cur.started)})
	s.failed = s.prefix()
	s.log.Printf("失败：%s", s.failed)
	s.clearLine()
	fmt.Fprintf(s.out, "%s %s\n", s.failed, s.color("1;31", "✗"))
	s.cur = nil
}

// FailedStage 返回失败的步骤，例如“[8/9] 恢复数据”；没有失败时为空。
func (s *Screen) FailedStage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// Detail 写进日志；--verbose 时也显示在屏幕上。
func (s *Screen) Detail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.log.Printf("%s", msg)
	if !s.verbose {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLine()
	fmt.Fprintf(s.out, "    %s\n", msg)
}

// Warn 显示一条警告，写进日志，并汇总到最终结果。
func (s *Screen) Warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.log.Printf("警告：%s", msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLine()
	fmt.Fprintf(s.out, "    %s %s\n", s.color("1;33", "!"), msg)
	s.warnings = append(s.warnings, msg)
}

// Error 记录失败原因和建议，并在屏幕上显示。只有第一条会出现在结果摘要里。
func (s *Screen) Error(reason, hint string) {
	if hint != "" {
		s.log.Printf("失败原因：%s（建议：%s）", reason, hint)
	} else {
		s.log.Printf("失败原因：%s", reason)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil {
		s.failure = &Failure{Reason: reason, Hint: hint}
	}
	s.clearLine()
	fmt.Fprintf(s.out, "%s %s\n", s.color("1;31", "✗"), reason)
	if hint != "" {
		fmt.Fprintf(s.out, "  建议：%s\n", hint)
	}
}

// Failure 返回第一条失败原因；没有失败时为 nil。
func (s *Screen) Failure() *Failure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

// Warnings 返回已经显示过的警告。
func (s *Screen) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warnings...)
}

// Println 输出普通文字（菜单、说明等），会先清掉进度行。
func (s *Screen) Println(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLine()
	fmt.Fprintln(s.out, text)
}

// Note 在步骤行里显示一条状态（如健康检查等待中）。非终端时最多每 10 秒一行。
func (s *Screen) Note(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tty {
		s.draw(s.label(text))
		return
	}
	if now := s.now(); now.Sub(s.lastNote) >= 10*time.Second {
		s.lastNote = now
		fmt.Fprintf(s.out, "[进度] %s\n", s.label(text))
	}
}

// Count 在步骤行里显示“第几个/共几个”，例如逐个分析容器时。
func (s *Screen) Count(label string, current, total int, item string) {
	line := fmt.Sprintf("%s %d/%d", label, current, total)
	if item != "" {
		line += "（" + item + "）"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tty {
		s.draw(s.label(line))
	} else if current == total || current%20 == 0 {
		fmt.Fprintf(s.out, "[进度] %s\n", s.label(line))
	}
}

// Report 是任务结束时的结果摘要。
type Report struct {
	Title   string   // 例如“迁入结果”
	Result  string   // 例如“✅ 迁入成功”
	Lines   []string // 结果下面的其他行
	Elapsed time.Duration
	Success bool
	LogPath string   // 失败时显示的日志位置
	LogTail []string // 失败时显示的最近日志
}

// PrintReport 打印结果摘要。成功时附上各步骤耗时；失败时附上原因、建议和日志位置。
func (s *Screen) PrintReport(r Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLine()
	var b strings.Builder
	middle := " DockHop " + r.Title + " "
	b.WriteString("\n" + strings.Repeat("━", 10) + middle + strings.Repeat("━", 10) + "\n")
	b.WriteString("结果：" + r.Result + "\n")
	for _, l := range r.Lines {
		b.WriteString(l + "\n")
	}
	if r.Elapsed > 0 {
		b.WriteString("耗时：" + Elapsed(r.Elapsed) + "\n")
	}
	if r.Success && len(s.timings) > 0 {
		parts := make([]string, 0, len(s.timings))
		for _, t := range s.timings {
			parts = append(parts, t.Title+" "+Elapsed(t.Elapsed))
		}
		b.WriteString("各步骤耗时：" + strings.Join(parts, " · ") + "\n")
	}
	if len(s.warnings) > 0 {
		seen := map[string]bool{}
		var uniq []string
		for _, w := range s.warnings {
			if !seen[w] {
				seen[w] = true
				uniq = append(uniq, w)
			}
		}
		fmt.Fprintf(&b, "警告：%d 条\n", len(s.warnings))
		for i, w := range uniq {
			if i == 10 {
				fmt.Fprintf(&b, "  · ……还有 %d 条，见日志\n", len(uniq)-10)
				break
			}
			b.WriteString("  · " + w + "\n")
		}
	}
	if !r.Success {
		if s.failure != nil {
			b.WriteString("原因：" + s.failure.Reason + "\n")
			if s.failure.Hint != "" {
				b.WriteString("建议：" + s.failure.Hint + "\n")
			}
		}
		if r.LogPath != "" {
			b.WriteString("日志：" + r.LogPath + "\n")
			b.WriteString("      反馈问题时请附上这个文件（密钥和密码已自动隐藏），或运行 dockhop diagnose 生成诊断文件。\n")
		}
		if len(r.LogTail) > 0 {
			b.WriteString("最近日志：\n")
			for _, l := range r.LogTail {
				b.WriteString("  " + l + "\n")
			}
		}
	}
	b.WriteString(strings.Repeat("━", 20+displayWidth(middle)) + "\n")
	fmt.Fprint(s.out, b.String())
}
