package ui

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// 进度行的三种样子：
//
//	知道总量：  [5/7] 保存镜像 ██████░░░░ 58% · 1.2GB/2.1GB · 45MB/s · 剩余约 20 秒
//	不知道总量：[4/7] 停机并打包 · 卷 mysql_data ⠹ 已处理 1.2GB · 45MB/s · 1 分 3 秒
//	没有字节数：[9/9] 启动服务 · nginx ⠹ 1 分 3 秒
//
// 长时间没有进展时在后面加一句说明（hint），让用户知道程序仍在运行、在等什么。

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// ProgressState 是画一行进度需要的全部信息，方便单独测试。
type ProgressState struct {
	Label   string
	Current int64
	Total   int64 // 0 表示不知道总量
	Elapsed time.Duration
	Rate    int64 // 字节/秒，0 表示不显示
	Spinner string
	Hint    string
}

// RenderProgress 生成一行进度文字（不含换行）。只在确实知道总量时显示百分比，
// 不用“假进度条”误导用户。
func RenderProgress(p ProgressState) string {
	var b strings.Builder
	cur, total := p.Current, p.Total
	if cur < 0 {
		cur = 0
	}
	switch {
	case total > 0:
		if cur > total {
			cur = total
		}
		percent := int(cur * 100 / total)
		// 总量常常是估算值，命令真正结束前不显示 100%。
		if percent > 99 {
			percent = 99
		}
		filled := percent * 20 / 100
		fmt.Fprintf(&b, "%s %s%s %d%% · %s/%s", p.Label,
			strings.Repeat("█", filled), strings.Repeat("░", 20-filled),
			percent, HumanBytes(cur), HumanBytes(total))
		if p.Rate > 0 {
			b.WriteString(" · " + HumanBytes(p.Rate) + "/s")
		}
		if p.Rate > 0 && total > cur {
			if left := time.Duration((total-cur)/p.Rate) * time.Second; left > 0 {
				b.WriteString(" · 剩余约 " + Elapsed(left))
			} else {
				b.WriteString(" · 即将完成")
			}
		} else {
			b.WriteString(" · " + Elapsed(p.Elapsed))
		}
	case cur > 0:
		b.WriteString(p.Label)
		if p.Spinner != "" {
			b.WriteString(" " + p.Spinner)
		}
		b.WriteString(" 已处理 " + HumanBytes(cur))
		if p.Rate > 0 {
			b.WriteString(" · " + HumanBytes(p.Rate) + "/s")
		}
		b.WriteString(" · " + Elapsed(p.Elapsed))
	default:
		b.WriteString(p.Label)
		if p.Spinner != "" {
			b.WriteString(" " + p.Spinner)
		}
		b.WriteString(" " + Elapsed(p.Elapsed))
	}
	if p.Hint != "" {
		b.WriteString(" · " + p.Hint)
	}
	return b.String()
}

// Progress 显示一个耗时操作的进度：终端里每秒原地刷新，非终端每 10 秒输出一行。
type Progress struct {
	s       *Screen
	label   string
	hint    string
	current atomic.Int64
	total   atomic.Int64
	started time.Time
	stop    chan struct{}
	done    chan struct{}
}

// StallAfter 是多久没有进展后显示说明。
var StallAfter = 30 * time.Second

// StartProgress 开始显示进度。total 为 0 表示不知道总量；hint 是长时间没有
// 进展时显示的说明，为空时显示“仍在进行，请耐心等待”。用完必须调用 Done。
func (s *Screen) StartProgress(label string, total int64, hint string) *Progress {
	p := &Progress{s: s, label: label, hint: hint, started: s.now(),
		stop: make(chan struct{}), done: make(chan struct{})}
	p.total.Store(total)
	s.log.Printf("开始：%s", label)
	interval := 10 * time.Second
	if s.tty {
		interval = time.Second
	}
	go p.loop(interval)
	return p
}

// Add 增加已处理的字节数。
func (p *Progress) Add(n int64) { p.current.Add(n) }

// Set 设置已处理的字节数。
func (p *Progress) Set(n int64) { p.current.Store(n) }

// SetTotal 在开始后才知道总量时（如下载拿到响应头）设置总量。
func (p *Progress) SetTotal(n int64) { p.total.Store(n) }

// Write 让 Progress 可以直接接在 io.Copy 的数据流上计数。
func (p *Progress) Write(b []byte) (int, error) {
	p.current.Add(int64(len(b)))
	return len(b), nil
}

func (p *Progress) loop(interval time.Duration) {
	defer close(p.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastBytes, rate int64
	lastAt, changedAt := p.started, p.started
	tick := 0
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
		now := p.s.now()
		cur := p.current.Load()
		if dt := now.Sub(lastAt).Seconds(); dt > 0 {
			sample := int64(float64(cur-lastBytes) / dt)
			if sample < 0 {
				sample = 0
			}
			// 速度取指数平滑，避免网络抖动时数字跳得太厉害。
			if rate == 0 {
				rate = sample
			} else {
				rate = (rate*7 + sample*3) / 10
			}
		}
		if cur != lastBytes {
			changedAt = now
		}
		lastBytes, lastAt = cur, now
		hint := ""
		if now.Sub(changedAt) >= StallAfter {
			hint = p.hint
			if hint == "" {
				hint = "仍在进行，请耐心等待"
			}
		}
		st := ProgressState{Current: cur, Total: p.total.Load(), Elapsed: now.Sub(p.started), Rate: rate, Hint: hint}
		p.s.mu.Lock()
		st.Label = p.s.label(p.label)
		if p.s.tty {
			st.Spinner = spinnerFrames[tick%len(spinnerFrames)]
			p.s.draw(RenderProgress(st))
		} else {
			fmt.Fprintf(p.s.out, "[进度] %s\n", RenderProgress(st))
		}
		p.s.mu.Unlock()
		tick++
	}
}

// Done 结束进度显示。err 不为空时只记日志：失败原因由步骤结果统一说明，
// 屏幕上不重复报错。
func (p *Progress) Done(err error) {
	close(p.stop)
	<-p.done
	elapsed := p.s.now().Sub(p.started)
	cur := p.current.Load()
	extra := ""
	if cur > 0 {
		extra = " · " + HumanBytes(cur)
	}
	if err != nil {
		p.s.log.Printf("失败：%s（%v，耗时 %s%s）", p.label, err, Elapsed(elapsed), extra)
	} else {
		p.s.log.Printf("完成：%s · %s%s", p.label, Elapsed(elapsed), extra)
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if p.s.tty {
		// 回到步骤行，不留下已完成子步骤的进度文字。
		if p.s.cur != nil {
			p.s.draw(p.s.prefix() + " …")
		} else {
			p.s.clearLine()
		}
	}
}
