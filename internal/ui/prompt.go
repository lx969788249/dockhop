package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ErrNotInteractive 表示没有终端可以提问（定时任务、管道、nohup 等）。
var ErrNotInteractive = errors.New("没有可以输入的终端")

// ErrCancelled 表示用户取消了（回答问题时，或者还没改动任何东西就按了 Ctrl+C），取消的说明
// 已经显示过了。
var ErrCancelled = errors.New("已取消")

// Canceled 表示 err 是用户在回答问题时按了 Ctrl+C（程序被结束）或 Ctrl+D（输入结束）。
func Canceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, io.EOF)
}

// Prompter 向用户提问。in 为 nil 表示不能交互。
type Prompter struct {
	in    *bufio.Reader
	out   io.Writer
	ctx   context.Context
	lines chan readResult // 正在读的那一行；被 Ctrl+C 打断时留给下一次提问
	tty   int             // 输入是终端时是它的文件描述符（Keys 要用），否则为 -1
}

type readResult struct {
	line string
	err  error
}

// NewPrompter 创建提问器。in 通常是 /dev/tty；不能交互时传 nil。
func NewPrompter(in io.Reader, out io.Writer) *Prompter {
	p := &Prompter{out: out, tty: -1}
	if in != nil {
		p.in = bufio.NewReader(in)
		if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			p.tty = int(f.Fd())
		}
	}
	return p
}

// WithContext 让提问能被 Ctrl+C 打断：程序接管了 Ctrl+C 信号，读终端又会一直等到回车，
// 不这样做的话，按 Ctrl+C 要再按一次回车才有反应。
func (p *Prompter) WithContext(ctx context.Context) *Prompter {
	p.ctx = ctx
	return p
}

// Interactive 表示能否向用户提问。
func (p *Prompter) Interactive() bool { return p != nil && p.in != nil }

func (p *Prompter) readLine() (string, error) {
	if !p.Interactive() {
		return "", ErrNotInteractive
	}
	if p.ctx == nil {
		return p.read()
	}
	if p.lines == nil {
		p.lines = make(chan readResult, 1)
		go func() {
			line, err := p.read()
			p.lines <- readResult{line, err}
		}()
	}
	select {
	case res := <-p.lines:
		p.lines = nil
		return res.line, res.err
	case <-p.ctx.Done():
		return "", p.ctx.Err()
	}
}

func (p *Prompter) read() (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Choose 列出选项让用户选一个，返回从 1 开始的序号。没有默认值，一定要输入序号：直接回车
// 不算，免得多带的一个回车（例如粘贴命令时）替用户做了选择。选项写成“  1. 迁出”。
func (p *Prompter) Choose(options []string) (int, error) {
	return p.choose(options, "  ", "%d. ")
}

// Menu 和 Choose 一样，用在首页：和首页的其他行一样缩进 1 格，选项写成“ [1]  迁出”。
func (p *Prompter) Menu(options []string) (int, error) {
	return p.choose(options, " ", "[%d]  ")
}

// choose 列出选项、读序号。indent 是选项和提示的缩进，number 是序号的写法。
func (p *Prompter) choose(options []string, indent, number string) (int, error) {
	if !p.Interactive() {
		return 0, ErrNotInteractive
	}
	for i, o := range options {
		fmt.Fprintf(p.out, "%s%s%s\n", indent, fmt.Sprintf(number, i+1), o)
	}
	for {
		fmt.Fprint(p.out, indent+"请输入数字：")
		line, err := p.readLine()
		if err != nil {
			return 0, err
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return n, nil
		}
		fmt.Fprintf(p.out, "%s请输入 1 到 %d 之间的数字。\n", indent, len(options))
	}
}

// Confirm 问一个是非题；直接回车选 def。
func (p *Prompter) Confirm(question string, def bool) (bool, error) {
	if !p.Interactive() {
		return false, ErrNotInteractive
	}
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	for {
		fmt.Fprintf(p.out, "%s %s ", question, hint)
		line, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(line) {
		case "":
			return def, nil
		case "y", "yes", "是":
			return true, nil
		case "n", "no", "否":
			return false, nil
		}
		fmt.Fprintln(p.out, "请输入 y 或 n。")
	}
}

// Ask 让用户输入一段文字；valid 不为 nil 时会反复询问直到通过检查。
func (p *Prompter) Ask(label string, valid func(string) error) (string, error) {
	if !p.Interactive() {
		return "", ErrNotInteractive
	}
	for {
		fmt.Fprint(p.out, label)
		line, err := p.readLine()
		if err != nil {
			return "", err
		}
		if valid == nil {
			return line, nil
		}
		if err := valid(line); err != nil {
			fmt.Fprintln(p.out, err.Error())
			continue
		}
		return line, nil
	}
}
