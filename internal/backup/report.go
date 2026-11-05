package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/lx969788249/dockhop/internal/ui"
)

// 结果摘要：迁出成功、新服务器只迁入了一部分、失败或被中断时分别怎么说，停机多久，停下的
// 容器恢复了没有。还没改动就取消时只说一句已取消。

// report 打印结果摘要。
func (r *run) report(ctx context.Context, err error) error {
	// 还没改动就取消了（回答问题时按 Ctrl+C、Ctrl+D，或者检查、打包时按 Ctrl+C）：什么都没做，
	// 不用摆出一整块失败结果。按没按 Ctrl+C 看 ctx：被打断的那一步返回什么错误都有可能。
	if (ui.Canceled(err) || ctx.Err() != nil) && r.toRestore == 0 && r.server == nil {
		r.Screen.Println("\n已取消，这台服务器上什么都没有改动。")
		return ui.ErrCancelled
	}
	interrupted := ctx.Err() != nil && r.server == nil
	success := err == nil && !interrupted
	lines := append([]string(nil), r.summary...)
	hostLine := ""
	switch {
	case r.toRestore > 0 && r.restored < r.toRestore:
		hostLine = fmt.Sprintf("这台服务器：停下的 %d 个容器只恢复了 %d 个，请立即人工检查", r.toRestore, r.restored)
		success = false
	case r.toRestore > 0 && r.downtime > 0:
		hostLine = "这台服务器：打包时停了" + downtimeText(r.downtime) + "，服务都已恢复运行"
	case r.toRestore > 0:
		hostLine = "这台服务器：停下的容器都已恢复原来的状态"
	case success:
		hostLine = "这台服务器：服务一直在运行，没有改动"
	}
	if hostLine != "" {
		lines = append(lines, hostLine)
	}
	rep := ui.Report{Title: "迁出结果", Lines: lines, Elapsed: time.Since(r.started), Success: success}
	switch {
	case success && r.status == "success":
		rep.Result = "✅ 迁移完成，新服务器已迁入成功"
	case success:
		switch r.status {
		case "partial":
			rep.Result = "⚠️ 已结束传输，新服务器只迁入了一部分"
		case "failed":
			rep.Result = "⚠️ 已结束传输，新服务器迁入失败"
		default:
			rep.Result = "⚠️ 已结束传输，没有收到新服务器迁入成功的消息"
		}
		rep.Next = []string{"要迁移时重新运行迁出，会生成一个新的链接"}
	case interrupted:
		rep.Result = "⚠️ 迁出已中断"
	default:
		rep.Result = "❌ 迁出失败"
		r.Screen.Fail()
	}
	if !success {
		rep.LogPath = r.Log.Path()
	}
	r.Screen.PrintReport(rep)
	if success {
		return nil // 用户自己结束传输也算正常结束
	}
	return ui.ErrReported
}

// downtimeText 是停机时长；不到一秒时不写“0 秒”。
func downtimeText(d time.Duration) string {
	if d < time.Second {
		return "不到 1 秒"
	}
	return " " + ui.Elapsed(d)
}
