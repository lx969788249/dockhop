package ui

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "用当前输出更新 testdata 里的快照")

// golden 把输出和 testdata 里的快照比较；带 -update 运行时改为写入快照。
func golden(t *testing.T, name string, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取快照 %s 失败（首次运行请加 -update）：%v", path, err)
	}
	if got != string(want) {
		t.Errorf("屏幕输出和快照 %s 不一致\n--- 实际 ---\n%s\n--- 快照 ---\n%s", path, got, want)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newScreen(tty bool) (*Screen, *bytes.Buffer, *fakeClock) {
	var out bytes.Buffer
	clock := &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	s := New(&out, tty, nil)
	s.now = clock.now
	return s, &out, clock
}

// 一次成功的迁入：步骤、警告、计数、结果摘要。
func scriptSuccess(s *Screen, clock *fakeClock) {
	s.Begin(1, 9, "检查环境")
	clock.advance(1 * time.Second)
	s.End("Docker 29.2.1")
	s.Begin(2, 9, "迁移前预检")
	s.Warn("新服务器的 Docker（20.10.24）比旧服务器（29.2.1）旧，一般不影响迁移")
	clock.advance(2 * time.Second)
	s.End("5 个容器 · 需要约 1.2GB")
	s.Begin(3, 9, "读取配置")
	s.Count("分析容器", 3, 3, "nextcloud-app")
	clock.advance(75 * time.Second)
	s.End("")
	s.PrintReport(Report{
		Title:   "迁入结果",
		Result:  "✅ 迁入成功",
		Lines:   []string{"容器：5 个（运行 5 / 暂停 0 / 停止 0）"},
		Elapsed: 78 * time.Second,
		Success: true,
	})
}

// 一次失败的迁入：失败原因只保留第一条，附日志位置；建议只在结果摘要里写一次。
func scriptFailure(s *Screen, clock *fakeClock) {
	s.Begin(8, 9, "恢复数据")
	clock.advance(4 * time.Second)
	s.Error("数据卷 mysql_data 写入失败：磁盘空间不足", "执行 df -h 查看剩余空间，清理后重试")
	s.Error("第二条原因不应出现在摘要里", "")
	s.Fail()
	s.PrintReport(Report{
		Title:   "迁入结果",
		Result:  "❌ 迁入失败，迁入的内容已全部删除",
		Lines:   []string{"失败步骤：" + s.FailedStage(), "新服务器：回到了迁入前的样子"},
		Elapsed: 4 * time.Second,
		LogPath: "/root/.dockhop/logs/in-20261021-120000-123.log",
	})
}

func TestScreenSnapshots(t *testing.T) {
	for _, tty := range []bool{false, true} {
		mode := "plain"
		if tty {
			mode = "tty"
		}
		s, out, clock := newScreen(tty)
		scriptSuccess(s, clock)
		golden(t, "success_"+mode, out.String())

		s, out, clock = newScreen(tty)
		scriptFailure(s, clock)
		golden(t, "failure_"+mode, out.String())
	}
}

func TestFailureKeepsFirstReason(t *testing.T) {
	s, _, clock := newScreen(false)
	scriptFailure(s, clock)
	f := s.Failure()
	if f == nil || !strings.Contains(f.Reason, "mysql_data") {
		t.Fatalf("应保留第一条失败原因，实际：%+v", f)
	}
	if got := s.FailedStage(); got != "[8/9] 恢复数据" {
		t.Fatalf("失败步骤：%q", got)
	}
}

func TestVerboseDetail(t *testing.T) {
	s, out, _ := newScreen(false)
	s.Detail("只写日志")
	if out.Len() != 0 {
		t.Fatalf("默认不应显示细节：%q", out.String())
	}
	s.SetVerbose(true)
	s.Detail("也显示在屏幕上")
	if !strings.Contains(out.String(), "    也显示在屏幕上\n") {
		t.Fatalf("--verbose 时应显示细节：%q", out.String())
	}
}

func TestRenderProgress(t *testing.T) {
	cases := []struct {
		name string
		in   ProgressState
		want string
	}{
		{"知道总量", ProgressState{Label: "[5/7] 保存镜像", Current: 600 << 20, Total: 1000 << 20, Elapsed: 10 * time.Second, Rate: 50 << 20},
			"[5/7] 保存镜像 ████████████░░░░░░░░ 60% · 600MB/1000MB · 50MB/s · 剩余约 8 秒"},
		{"超过总量也不显示 100%", ProgressState{Label: "下载", Current: 2000, Total: 1000, Elapsed: 3 * time.Second},
			"下载 ███████████████████░ 99% · 1000B/1000B · 3 秒"},
		{"不知道总量", ProgressState{Label: "卷 data", Current: 3 << 30, Elapsed: 65 * time.Second, Rate: 45 << 20, Spinner: "⠹"},
			"卷 data ⠹ 已处理 3GB · 45MB/s · 1 分 5 秒"},
		{"没有字节数，长时间没有进展", ProgressState{Label: "[9/9] 启动服务 · nginx", Elapsed: 40 * time.Second, Hint: "正在等待健康检查"},
			"[9/9] 启动服务 · nginx 40 秒 · 正在等待健康检查"},
	}
	for _, c := range cases {
		if got := RenderProgress(c.in); got != c.want {
			t.Errorf("%s：\n实际 %q\n期望 %q", c.name, got, c.want)
		}
	}
}

func TestProgressLifecycle(t *testing.T) {
	s, out, _ := newScreen(true)
	s.Begin(4, 7, "停机并打包")
	p := s.StartProgress("卷 data", 0, "")
	p.Write(make([]byte, 1024))
	p.Done(nil)
	s.End("1 个卷")
	if !strings.HasSuffix(out.String(), "[4/7] 停机并打包 \033[32m✓\033[0m 1 个卷\n") {
		t.Fatalf("进度结束后应回到步骤行：%q", out.String())
	}
}

// 终端窄时，进度行要缩到一行以内：折行后 \r 回不到上一行，旧内容会留在屏幕上。
func TestFitProgress(t *testing.T) {
	p := ProgressState{Label: "[4/6] 停机打包数据 · 目录 /opt/1panel/apps/halo/halo/data", Current: 178 << 20, Total: 292 << 20,
		Elapsed: 3 * time.Second, Rate: 64 << 20}
	for _, w := range []int{120, 79, 60, 40, 20} {
		got := FitProgress(p, w)
		if displayWidth(got) > w {
			t.Errorf("宽度 %d：超出了（%d 列）%q", w, displayWidth(got), got)
		}
		if w >= 60 && !strings.Contains(got, "60%") {
			t.Errorf("宽度 %d：应保留百分比 %q", w, got)
		}
	}
	if got := FitProgress(p, 200); got != RenderProgress(p) {
		t.Errorf("够宽时应和 RenderProgress 一样：%q", got)
	}
	if got := FitProgress(p, 79); !strings.HasPrefix(got, "[4/6] 停机打包数据") {
		t.Errorf("截短的应是后面的对象，不是步骤名：%q", got)
	}
}

// 原地刷新的那一行（步骤标题、计数、说明）也不能超过终端宽度。
func TestDrawFitsWidth(t *testing.T) {
	s, out, _ := newScreen(true)
	s.SetWidth(func() int { return 30 })
	s.Begin(3, 6, "读取配置")
	s.Note("统计目录大小 · /opt/1panel/apps/halo/halo/data/very/long")
	for _, part := range strings.Split(out.String(), "\r") {
		line := strings.TrimSuffix(strings.TrimPrefix(part, "\033[K"), "\033[K")
		if displayWidth(line) > 29 {
			t.Errorf("超出终端宽度：%q", line)
		}
	}
}

// 步骤里问过问题的，这一步结束时清屏重画：只留下程序名字、之前的步骤结果和警告，不留菜单。
func TestRedrawAfterQuestions(t *testing.T) {
	s, out, _ := newScreen(true)
	s.SetBanner("DockHop\n\n")
	s.Begin(1, 6, "检查环境")
	s.End("Docker 29")
	s.Begin(2, 6, "选择要迁移的服务")
	s.Hold()
	s.Println("    1) web")
	s.Warn("自动加入 db")
	s.End("web")
	screen := out.String()[strings.LastIndex(out.String(), clearScreen)+len(clearScreen):]
	want := "DockHop\n\n[1/6] 检查环境 \033[32m✓\033[0m Docker 29\n    \033[33m!\033[0m 自动加入 db\n" +
		"[2/6] 选择要迁移的服务 \033[32m✓\033[0m web\n"
	if screen != want {
		t.Errorf("清屏重画后：\n实际 %q\n期望 %q", screen, want)
	}
	// 没问过问题的步骤不清屏。
	s.Begin(3, 6, "读取配置")
	s.End("")
	if strings.Count(out.String(), clearScreen) != 1 {
		t.Errorf("没问过问题的步骤不应清屏：%q", out.String())
	}
	// 不是终端时、--verbose 时从不清屏。
	p, pout, _ := newScreen(false)
	v, vout, _ := newScreen(true)
	v.SetVerbose(true)
	for _, x := range []*Screen{p, v} {
		x.Begin(2, 6, "选择要迁移的服务")
		x.Hold()
		x.End("web")
	}
	if strings.Contains(pout.String(), clearScreen) || strings.Contains(vout.String(), clearScreen) {
		t.Errorf("不是终端时、--verbose 时不应清屏：%q %q", pout.String(), vout.String())
	}
}

// Start 在终端里先清屏再显示程序名字；不是终端时只写名字。
func TestStart(t *testing.T) {
	s, out, _ := newScreen(true)
	s.SetBanner("DockHop\n")
	s.Start()
	if out.String() != clearScreen+"DockHop\n" {
		t.Errorf("终端里：%q", out.String())
	}
	p, pout, _ := newScreen(false)
	p.SetBanner("DockHop\n")
	p.Start()
	p.Refresh()
	if pout.String() != "DockHop\n" {
		t.Errorf("不是终端时：%q", pout.String())
	}
}

// Error 当场只写原因（建议在结果摘要里）；Problem 用在没有结果摘要的地方，当场写出建议。
func TestErrorAndProblem(t *testing.T) {
	s, out, _ := newScreen(false)
	s.Error("磁盘满了", "清理磁盘")
	if out.String() != "✗ 磁盘满了\n" || s.Failure().Hint != "清理磁盘" {
		t.Errorf("Error：%q %+v", out.String(), s.Failure())
	}
	p, pout, _ := newScreen(false)
	p.Problem("连不上 Docker", "systemctl start docker")
	if pout.String() != "✗ 连不上 Docker\n  建议：systemctl start docker\n" {
		t.Errorf("Problem：%q", pout.String())
	}
}

// 颜色只上色、不加粗：有的终端里加粗的中文会显示成乱码。
func TestColorsNotBold(t *testing.T) {
	s, _, _ := newScreen(true)
	for _, c := range []string{s.Green("链接"), s.Orange("⚠ 请不要关闭这个窗口")} {
		if strings.Contains(c, "\033[1") {
			t.Errorf("不应加粗：%q", c)
		}
	}
	p, _, _ := newScreen(false)
	if p.Green("链接") != "链接" {
		t.Error("不是终端时不应上色")
	}
}

func TestNames(t *testing.T) {
	if got := Names([]string{"a", "b"}); got != "a、b" {
		t.Errorf("两个：%q", got)
	}
	if got := Names([]string{"a", "b", "c", "d", "e", "f", "g"}); got != "a、b、c、d、e 等 7 项" {
		t.Errorf("七个：%q", got)
	}
}

func TestCanceled(t *testing.T) {
	if !Canceled(context.Canceled) || !Canceled(fmt.Errorf("读取：%w", io.EOF)) || Canceled(errors.New("别的错误")) {
		t.Error("Canceled 判断不对")
	}
}

func TestInfoIsNotWarning(t *testing.T) {
	s, out, _ := newScreen(false)
	s.Info("自动加入 db：web 依赖它，必须一起迁移")
	if len(s.Warnings()) != 0 || !strings.Contains(out.String(), "    · 自动加入 db") {
		t.Fatalf("说明不算警告：%q %v", out.String(), s.Warnings())
	}
}

func TestShortPathAndTruncate(t *testing.T) {
	if got := ShortPath("/opt/1panel/apps/halo/halo/data"); got != "…/halo/data" {
		t.Errorf("ShortPath 长路径：%q", got)
	}
	if got := ShortPath("/root/site"); got != "/root/site" {
		t.Errorf("ShortPath 短路径不变：%q", got)
	}
	if got := Truncate("停机打包数据", 7); got != "停机打…" {
		t.Errorf("Truncate 中文：%q", got)
	}
	if got := Truncate("abc", 3); got != "abc" {
		t.Errorf("Truncate 不用截：%q", got)
	}
}

// 按 Ctrl+C 时，正在等输入的提问要马上返回，不用再按一次回车。
func TestPromptCancel(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p := NewPrompter(r, io.Discard).WithContext(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := p.Choose([]string{"a", "b"}, 1)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("应返回 context.Canceled：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后提问没有返回")
	}
}

func TestFormat(t *testing.T) {
	bytesCases := map[int64]string{0: "0B", 1023: "1023B", 1536: "1.5KB", 10 * 1024: "10KB", 1 << 30: "1GB", 1288490188: "1.2GB", -5: "0B"}
	for in, want := range bytesCases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q，期望 %q", in, got, want)
		}
	}
	durCases := map[time.Duration]string{0: "0 秒", 59 * time.Second: "59 秒", 61 * time.Second: "1 分 1 秒", 3723 * time.Second: "1 小时 2 分 3 秒"}
	for in, want := range durCases {
		if got := Elapsed(in); got != want {
			t.Errorf("Elapsed(%v) = %q，期望 %q", in, got, want)
		}
	}
	if got := CJKSpaced("nginx"); got != " nginx " {
		t.Errorf("CJKSpaced 英文：%q", got)
	}
	if got := CJKSpaced("其他程序"); got != "其他程序" {
		t.Errorf("CJKSpaced 中文：%q", got)
	}
}

func TestPrompter(t *testing.T) {
	var out bytes.Buffer
	p := NewPrompter(strings.NewReader("9\nabc\n2\n\nn\n"), &out)
	n, err := p.Choose([]string{"改用空闲端口 8361（推荐）", "取消"}, 1)
	if err != nil || n != 2 {
		t.Fatalf("Choose = %d, %v", n, err)
	}
	if !strings.Contains(out.String(), "请输入 1 到 2 之间的数字。") {
		t.Fatalf("输入无效时应提示：%q", out.String())
	}
	n, err = p.Choose([]string{"a", "b"}, 1)
	if err != nil || n != 1 {
		t.Fatalf("直接回车应选默认：%d, %v", n, err)
	}
	ok, err := p.Confirm("现在启动它们吗？", true)
	if err != nil || ok {
		t.Fatalf("Confirm = %v, %v", ok, err)
	}
	none := NewPrompter(nil, &out)
	if _, err := none.Choose([]string{"a"}, 1); !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("不能交互时应返回 ErrNotInteractive：%v", err)
	}
}

func TestBanner(t *testing.T) {
	if got := Banner("1.0.1", false); got != "DockHop 1.0.1\n" {
		t.Errorf("不是终端时只写一行：%q", got)
	}
	got := Banner("1.0.1", true)
	if !strings.Contains(got, "|____/") || !strings.Contains(got, "DockHop 1.0.1") || !strings.Contains(got, Tagline) {
		t.Errorf("终端里应显示 logo、版本和一句话介绍：%q", got)
	}
}
