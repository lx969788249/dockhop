package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// OutOptions 是迁出的选项。
type OutOptions struct {
	Include    []string
	StopShared bool
	Port       int
	Host       string
}

// InOptions 是迁入的选项。
type InOptions struct {
	Link          string
	Yes           bool
	Existing      string // replace、skip、fail；空表示询问
	KeepSucceeded bool
}

// UsageError 是用户输入的命令或选项不对，退出码 2。
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func usageErr(format string, args ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

var flagNamePattern = regexp.MustCompile(`-+([A-Za-z0-9-]+)`)

// parseArgs 解析选项，允许选项和链接等参数前后穿插（dockhop in 链接 --yes）。
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, translateFlagError(err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		if args[0] == "--" {
			return append(positional, args[1:]...), nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// translateFlagError 把 flag 包的英文报错换成大白话。
func translateFlagError(err error) error {
	msg := err.Error()
	name := ""
	if m := flagNamePattern.FindStringSubmatch(msg[strings.LastIndex(msg, ":")+1:]); m != nil {
		name = "--" + m[1]
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		return err
	case strings.HasPrefix(msg, "flag provided but not defined"):
		return usageErr("不认识的选项 %s，运行 dockhop --help 查看可用的选项", name)
	case strings.HasPrefix(msg, "flag needs an argument"):
		return usageErr("选项 %s 后面需要写一个值", name)
	case strings.HasPrefix(msg, "invalid value"):
		if m := regexp.MustCompile(`invalid value "([^"]*)" for flag -+([A-Za-z0-9-]+)`).FindStringSubmatch(msg); m != nil {
			return usageErr("选项 --%s 的值不对：%s", m[2], m[1])
		}
		return usageErr("选项的值不对：%s", msg)
	case strings.HasPrefix(msg, "invalid boolean value"):
		if m := regexp.MustCompile(`invalid boolean value "([^"]*)" for -+([A-Za-z0-9-]+)`).FindStringSubmatch(msg); m != nil {
			return usageErr("选项 --%s 的值不对：%s（这个选项不需要写值）", m[2], m[1])
		}
		return usageErr("选项的值不对：%s", msg)
	case strings.HasPrefix(msg, "invalid boolean flag"):
		return usageErr("选项 %s 不需要写值", name)
	default:
		return usageErr("%s", msg)
	}
}

func parseOut(args []string) (OutOptions, error) {
	o := OutOptions{Port: 8080}
	fs := flag.NewFlagSet("out", flag.ContinueOnError)
	include := fs.String("include", "", "")
	fs.BoolVar(&o.StopShared, "stop-shared", false, "")
	fs.IntVar(&o.Port, "port", 8080, "")
	fs.StringVar(&o.Host, "host", "", "")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return o, err
	}
	if len(rest) > 0 {
		return o, usageErr("迁出不需要参数：%s。要只迁移某几个容器请用 --include 容器1,容器2", strings.Join(rest, " "))
	}
	for _, n := range strings.Split(*include, ",") {
		if n = strings.TrimSpace(n); n != "" {
			o.Include = append(o.Include, n)
		}
	}
	if *include != "" && len(o.Include) == 0 {
		return o, usageErr("--include 后面需要写容器名，多个用逗号分开")
	}
	if o.Port < 1 || o.Port > 65535 {
		return o, usageErr("--port 必须是 1 到 65535 之间的端口号")
	}
	return o, nil
}

func parseIn(args []string) (InOptions, error) {
	var o InOptions
	fs := flag.NewFlagSet("in", flag.ContinueOnError)
	fs.BoolVar(&o.Yes, "yes", false, "")
	fs.StringVar(&o.Existing, "existing", "", "")
	fs.BoolVar(&o.KeepSucceeded, "keep-succeeded", false, "")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return o, err
	}
	if len(rest) > 1 {
		return o, usageErr("只需要一个迁移链接，多出来的是：%s。链接里有特殊字符，记得用单引号括起来", strings.Join(rest[1:], " "))
	}
	if len(rest) == 1 {
		o.Link = rest[0]
	}
	switch o.Existing {
	case "", "replace", "skip", "fail":
	default:
		return o, usageErr("--existing 只能是 replace、skip 或 fail，不能是 %s", o.Existing)
	}
	return o, nil
}
