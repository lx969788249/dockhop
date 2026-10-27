package backup

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/ui"
)

func TestPortText(t *testing.T) {
	if got := portText(8080, 8080); got != "端口 8080" {
		t.Errorf("没换端口：%q", got)
	}
	if got := portText(8080, 8081); got != "端口 8081（8080 被占用）" {
		t.Errorf("换了端口：%q", got)
	}
}

// 交给用户的下一步：只有链接一样东西要复制；两条必须照做的提醒是橙色、不加粗
// （有的终端里加粗的中文会显示成乱码）；除了链接，每行在 80 列的终端里都不折行。
func TestInstructions(t *testing.T) {
	link, err := crypto.NewLink("127.0.0.1", 8080)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r := &run{Deps: Deps{Screen: ui.New(&out, true, nil), Prompt: ui.NewPrompter(strings.NewReader(""), &out)}}
	text := r.instructions(context.Background(), link)

	if strings.Count(text, link.String()) != 1 || strings.Contains(text, "dockhop.run) in") {
		t.Errorf("要复制的只能有链接一样：\n%s", text)
	}
	// 一行命令要写 https://：不写的话 curl 先走明文 HTTP，以 root 运行的脚本可能被路上的人改掉。
	for _, want := range []string{"\033[33m⚠ 请不要关闭这个窗口\033[0m", "\033[33m⚠ 请放行 TCP 8080 端口\033[0m", "选择「2) 迁入」",
		"bash <(curl -fsSL https://dockhop.run)"} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少 %q：\n%s", want, text)
		}
	}
	if strings.Contains(text, "\033[1") {
		t.Errorf("不应加粗：%q", text)
	}
	plain := regexp.MustCompile("\033\\[[0-9;]*m").ReplaceAllString(text, "")
	for _, line := range strings.Split(plain, "\n") {
		// 按 79 列算：⚠ 在有的终端里占两列。
		if line != link.String() && ui.Truncate(line, 79) != line {
			t.Errorf("这一行在 80 列的终端里会折行：%q", line)
		}
	}
}
