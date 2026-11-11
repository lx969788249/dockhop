package backup

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 提供下载（屏幕上的第 6 步）：开一个只认这次迁移链接的下载服务，告诉用户下一步怎么做，显示
// 新服务器的下载进度；新服务器报告迁入成功、或者用户按 Esc 再按回车时结束。

// serve 是最后一步：提供下载，直到新服务器迁入成功、用户按 Esc 和回车结束或程序被结束。
func (r *run) serve(ctx context.Context, ln net.Listener, link crypto.Link, bundlePath, planPath string, images []bundle.ImageFile) error {
	s := r.Screen
	live := &liveLine{s: s}
	defer live.stop()
	statusCh := make(chan transfer.Status, 4)
	completeCh := make(chan struct{}, 1)
	notify := func(e transfer.Event) {
		switch e.Kind {
		case transfer.DownloadProgress:
			live.downloading(e.Position, e.Size)
		case transfer.DownloadComplete:
			select {
			case completeCh <- struct{}{}:
			default:
			}
		case transfer.StatusReceived:
			statusCh <- e.Status
		}
	}
	srv, err := transfer.NewServer(ln, link, bundlePath, planPath, notify, r.Log.Printf)
	if err != nil {
		ln.Close()
		return &ui.Failure{Reason: "启动下载服务失败", Err: err}
	}
	files := map[string]string{}
	for _, f := range images {
		files[f.File] = filepath.Join(filepath.Dir(bundlePath), "images", f.File)
	}
	srv.SetImages(files)
	r.server = srv
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	s.End(portText(r.opts.Port, link.Port))

	s.Println(r.instructions(ctx, link))
	live.wait("等待新服务器连接")
	keys, restore := r.Prompt.Keys()
	defer restore()
	asked := false // 按了 Esc，在等回车确认
	for {
		select {
		case <-ctx.Done():
			s.Detail("收到结束信号，结束传输")
			return nil
		case k := <-keys:
			// 按 Esc 再按回车结束。q 和 Esc 一样：手机上的 SSH 软件不一定有 Esc 键，以前也是输入 q 结束。
			switch {
			case k == ui.KeyEsc || k == 'q' || k == 'Q':
				if !asked {
					asked = true
					s.Println("再按回车结束传输，链接随即失效；按其他键继续等待。")
				}
			case k == ui.KeyEnter && asked:
				s.Detail("用户按 Esc 和回车结束传输")
				return nil
			case k == ui.KeyEnter:
				s.Println("要结束传输，请先按 Esc，再按回车。")
			case asked:
				asked = false
				s.Println("继续等待。")
			}
		case <-completeCh:
			s.Println("新服务器已经下载完迁移包，正在迁入……")
			live.wait("新服务器正在迁入")
		case res := <-statusCh:
			r.status = res.Result
			if res.Result == "success" {
				live.stop()
				s.Println(s.Green("✓") + " 新服务器已迁入成功，传输自动结束。")
				return nil
			}
			msg := "新服务器迁入失败"
			if res.Result == "partial" {
				msg = "新服务器只迁入了一部分"
			}
			if res.Message != "" {
				msg += "：" + res.Message
			}
			s.Warn("%s。这里继续等着，在新服务器上处理好问题后，用同一个链接重新迁入即可", msg)
			live.wait("等待新服务器重新迁入")
		case err := <-serveErr:
			if err != nil {
				return &ui.Failure{Reason: "下载服务意外停止", Err: err, Hint: "重新运行迁出"}
			}
			return nil
		}
	}
}

// liveLine 是等待新服务器时屏幕最下面原地刷新的那一行：等待连接、下载进度、等待迁入结果，
// 让人一眼看出程序还在工作、窗口不能关。下载进度由下载服务在另一个 goroutine 里报告。
type liveLine struct {
	s          *ui.Screen
	mu         sync.Mutex
	prog       *ui.Progress
	inDownload bool // 正在显示下载进度
}

// wait 换成一行“正在等……”。只在终端里显示：日志里每 10 秒写一行“还在等”没有意义。
func (l *liveLine) wait(label string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopLocked()
	if l.s.IsTTY() {
		l.prog = l.s.StartProgress(label, 0, "请保持这个窗口开着")
	}
}

// downloading 显示新服务器的下载进度。
func (l *liveLine) downloading(pos, size int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.inDownload {
		l.stopLocked()
		l.prog = l.s.StartProgress("新服务器正在下载", size, "下载停住了：新服务器可能断开了，它会自动从断点继续")
		l.inDownload = true
	}
	l.prog.SetTotal(size) // 新服务器后来补下载镜像时，这一轮的总大小不一样
	l.prog.Set(pos)
}

func (l *liveLine) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopLocked()
}

func (l *liveLine) stopLocked() {
	if l.prog != nil {
		l.prog.Done(nil)
		l.prog = nil
	}
	l.inDownload = false
}

// instructions 是交给用户的下一步：要复制的只有迁移链接一样东西；新服务器上运行同一条命令、
// 选「迁入」、粘贴链接，和 README 写的一样。最要紧的两件事用橙色写：窗口不能关、端口要放行。
func (r *run) instructions(ctx context.Context, link crypto.Link) string {
	s := r.Screen
	rule := strings.Repeat("━", 56)
	var b strings.Builder
	b.WriteString("\n" + rule + "\n")
	b.WriteString("迁移链接（请完整复制）：\n\n")
	b.WriteString(s.Green(link.String()) + "\n\n")
	b.WriteString("下一步：\n")
	b.WriteString("  1. 登录新服务器，运行 bash <(curl -fsSL https://dockhop.run)\n")
	b.WriteString("  2. 选择「[2] 迁入」，粘贴上面的链接\n\n")
	// 这两行在 80 列的终端里不折行。
	b.WriteString(s.Orange("⚠ 请不要关闭这个窗口") + "：新服务器要从这里下载数据，迁入成功后这里会自动结束。\n")
	b.WriteString(s.Orange(fmt.Sprintf("⚠ 请放行 TCP %d 端口", link.Port)) + "：在云服务商的安全组或防火墙里放行，否则新服务器连不上。\n")
	if name, cmd := transfer.LocalFirewall(ctx, link.Port); name != "" {
		b.WriteString("  这台服务器开着 " + name + "，还要执行：" + cmd + "\n")
	}
	b.WriteString("· 链接里含有解密密钥，不要发给别人\n")
	if r.Prompt.Interactive() {
		b.WriteString("· 不想迁了：按 Esc 再按回车结束传输，链接随即失效\n")
	} else {
		b.WriteString(fmt.Sprintf("· 不想迁了：执行 kill -TERM %d 结束传输，链接随即失效\n", os.Getpid()))
	}
	b.WriteString(rule)
	return b.String()
}

// portText 是下载服务用的端口。要求的端口被占用、往后换了一个时说明原因：
// 照着默认端口提前在安全组里放行的人，要知道得改成放行这个端口。
func portText(want, got int) string {
	if got == want {
		return fmt.Sprintf("端口 %d", got)
	}
	return fmt.Sprintf("端口 %d（%d 被占用）", got, want)
}
