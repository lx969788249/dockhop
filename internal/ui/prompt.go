package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrNotInteractive 表示没有终端可以提问（定时任务、管道、nohup 等）。
var ErrNotInteractive = errors.New("没有可以输入的终端")

// Prompter 向用户提问。in 为 nil 表示不能交互。
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// NewPrompter 创建提问器。in 通常是 /dev/tty；不能交互时传 nil。
func NewPrompter(in io.Reader, out io.Writer) *Prompter {
	p := &Prompter{out: out}
	if in != nil {
		p.in = bufio.NewReader(in)
	}
	return p
}

// Interactive 表示能否向用户提问。
func (p *Prompter) Interactive() bool { return p != nil && p.in != nil }

func (p *Prompter) readLine() (string, error) {
	if !p.Interactive() {
		return "", ErrNotInteractive
	}
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Choose 列出选项让用户选一个，返回从 1 开始的序号；直接回车选 def。
func (p *Prompter) Choose(options []string, def int) (int, error) {
	if !p.Interactive() {
		return 0, ErrNotInteractive
	}
	for i, o := range options {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, o)
	}
	for {
		fmt.Fprintf(p.out, "请选择 [%d]：", def)
		line, err := p.readLine()
		if err != nil {
			return 0, err
		}
		if line == "" {
			return def, nil
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return n, nil
		}
		fmt.Fprintf(p.out, "请输入 1 到 %d 之间的数字。\n", len(options))
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
