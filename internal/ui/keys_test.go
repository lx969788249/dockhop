package ui

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// 终端送来的字节流切成键：方向键、功能键算一个；Esc 和后面的键一起到时分开；一串分几次到时
// 等到齐；一阵子没有新输入时，单独的 Esc 就是按了 Esc。
func TestKeyParser(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string // 每一段是一次读到的；"" 表示等了一会儿没有新输入
		want   []Key
	}{
		{"单按 Esc，再按回车", []string{"\x1b", "", "\r"}, []Key{KeyEsc, KeyEnter}},
		{"Esc 和回车一起到", []string{"\x1b\r"}, []Key{KeyEsc, KeyEnter}},
		{"Esc 和 q 一起到", []string{"\x1bq\n"}, []Key{KeyEsc, 'q', KeyEnter}},
		{"方向键", []string{"\x1b[A", "x"}, []Key{KeyOther, 'x'}},
		{"方向键分两次到", []string{"\x1b", "[A", "\r"}, []Key{KeyOther, KeyEnter}},
		{"带参数的功能键", []string{"\x1b[15~q"}, []Key{KeyOther, 'q'}},
		{"F1（SS3）", []string{"\x1bOP", "x"}, []Key{KeyOther, 'x'}},
		{"连着两个方向键再回车", []string{"\x1b[A\x1b[B\r"}, []Key{KeyOther, KeyOther, KeyEnter}},
		{"没到齐的一串等不到后续", []string{"\x1b[1", ""}, []Key{KeyOther}},
		{"Esc 后面没有别的", []string{"\x1b", ""}, []Key{KeyEsc}},
	}
	for _, c := range cases {
		var p keyParser
		var got []Key
		for _, ch := range c.chunks {
			if ch == "" {
				got = append(got, p.idle()...)
			} else {
				got = append(got, p.feed([]byte(ch))...)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s：实际 %v，期望 %v", c.name, got, c.want)
		}
	}
}

// 输入不是终端（管道、测试）时不能直接读按键：返回 nil，就像一直没人按键。
func TestKeysWithoutTerminal(t *testing.T) {
	for _, p := range []*Prompter{NewPrompter(strings.NewReader("q\n"), io.Discard), NewPrompter(nil, io.Discard)} {
		keys, restore := p.Keys()
		if keys != nil {
			t.Error("不是终端时应返回 nil")
		}
		restore()
	}
}
