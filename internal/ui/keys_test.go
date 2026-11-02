package ui

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// chunks 每次 Read 只给出一段，像终端那样：每按一次键到一次。
type chunks []string

func (c *chunks) Read(b []byte) (int, error) {
	if len(*c) == 0 {
		return 0, io.EOF
	}
	n := copy(b, (*c)[0])
	*c = (*c)[1:]
	return n, nil
}

func TestReadKey(t *testing.T) {
	in := chunks{"\x1b", "\r", "\x1b[A", "q", "\n", "\x1bOP", "x"}
	r := bufio.NewReader(&in)
	want := []Key{KeyEsc, KeyEnter, KeyOther, 'q', KeyEnter, KeyOther, 'x'}
	for i, w := range want {
		if k, err := readKey(r); err != nil || k != w {
			t.Fatalf("第 %d 个键：实际 %v（%v），期望 %v", i+1, k, err, w)
		}
	}
	if _, err := readKey(r); err != io.EOF {
		t.Errorf("读完以后应返回 io.EOF，实际 %v", err)
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
