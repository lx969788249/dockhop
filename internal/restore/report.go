package restore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 结果摘要：迁入成功、只迁入了一部分、失败或被中断时分别怎么说，并通知旧服务器结果（成功时
// 它会自动结束，失败时继续提供下载，方便重试）。

// countText 是迁入的容器和它们的状态。停止、暂停的容器是照旧服务器上的样子保留的，
// 单独说明，免得以为没启动成功。
func (r *run) countText() string {
	running, paused, stopped := 0, 0, 0
	for _, it := range r.items {
		switch {
		case it.c.State.Paused:
			paused++
		case it.running():
			running++
		default:
			stopped++
		}
	}
	if paused+stopped == 0 {
		if len(r.items) == 1 {
			return "容器正常运行"
		}
		return fmt.Sprintf("%d 个容器都在正常运行", len(r.items))
	}
	t := fmt.Sprintf("%d 个容器，%d 个正常运行", len(r.items), running)
	var kept []string
	if stopped > 0 {
		kept = append(kept, fmt.Sprintf("%d 个停止", stopped))
	}
	if paused > 0 {
		kept = append(kept, fmt.Sprintf("%d 个暂停", paused))
	}
	return t + "，" + strings.Join(kept, "、") + "（和旧服务器上一样）"
}

// servicesText 是迁入了哪些服务：独立容器写容器名、Compose 项目写项目名。
func (r *run) servicesText() string {
	var names []string
	seen := map[string]bool{}
	for _, it := range r.items {
		n := it.m.Name
		if it.m.Project != "" {
			n = it.m.Project
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return ui.Names(names)
}

func (r *run) report(ctx context.Context, err error) error {
	// 还没改动新服务器就取消了（回答问题、检查、下载时按 Ctrl+C）：不用摆出一整块失败结果。
	// 按没按 Ctrl+C 看 ctx：被打断的那一步返回什么错误都有可能。
	if (ui.Canceled(err) || ctx.Err() != nil) && r.j == nil {
		msg := "\n已取消，这台服务器上什么都没有改动。"
		if r.downloaded {
			msg += "已经下载的部分留着，用同一个链接重新迁入会接着下载。"
		}
		r.Screen.Println(msg)
		return ui.ErrCancelled
	}
	interrupted := ctx.Err() != nil
	success := err == nil && !interrupted
	if !success {
		r.Screen.Fail()
	}
	var lines []string
	status := transfer.Status{Result: "failed"}
	rep := ui.Report{Title: "迁入结果", Elapsed: time.Since(r.started)}
	untouched := "（还没有改动新服务器）"
	if !r.downloaded {
		untouched = "（还没有下载或改动任何东西）"
	}
	switch {
	case success:
		rep.Result = "✅ 迁入完成"
		lines = append(lines, "迁入的服务："+r.servicesText()+" · "+r.countText())
		if len(r.done) > 0 {
			lines = append(lines, "上次已经迁入："+strings.Join(r.done, "、"))
		}
		rep.Next = []string{
			"确认服务都正常（docker ps 可以查看容器）",
			"用域名访问的，把域名解析改到这台服务器",
			"确认没问题后，再停掉旧服务器上的服务（DockHop 不会删除旧服务器上的任何东西）",
		}
		status.Result = "success"
	case r.partial:
		ok, failed := r.splitGroups()
		rep.Result = "⚠️ 只迁入了一部分"
		lines = append(lines, "已经迁入、正常运行："+labels(ok))
		if len(r.done) > 0 {
			lines = append(lines, "上次已经迁入："+strings.Join(r.done, "、"))
		}
		if len(r.rollbackIssues) == 0 {
			lines = append(lines, "没有迁入（已删除）："+labels(failed))
		} else {
			lines = append(lines, "没有迁入、也没能删干净："+labels(failed))
			lines = append(lines, "重要：请先不要启动它们，按下面的情况人工处理")
			for _, p := range r.rollbackIssues {
				lines = append(lines, "  · "+p)
			}
			lines = append(lines, "回滚资料："+r.rbDir+"（下次运行迁入时会提示继续删除）")
		}
		if len(r.cleanupLeft) > 0 {
			left := "有些回滚资料没能删除：" + strings.Join(r.cleanupLeft, "；")
			if len(r.rollbackIssues) == 0 {
				left += "（下次运行迁入时会接着删除）"
			}
			lines = append(lines, left)
		}
		lines = append(lines, "处理好问题后，用同一个链接重新迁入：已经迁入的会跳过，只迁入剩下的")
		status.Result = "partial"
		status.Message = "没能启动：" + labels(failed)
	case r.committed:
		rep.Result = "⚠️ 服务和数据都已迁入，但清理没有完成"
		lines = append(lines, "待清理的回滚资料："+r.rbDir)
		status.Result = "success"
	case r.rollbackDone && len(r.rollbackIssues) == 0:
		if interrupted {
			rep.Result = "⚠️ 迁入已中断，迁入的内容已全部删除"
		} else {
			rep.Result = "❌ 迁入失败，迁入的内容已全部删除"
		}
		lines = append(lines, "新服务器：回到了迁入前的样子，原来的服务和数据都在")
	case r.rollbackDone || len(r.rollbackIssues) > 0:
		if interrupted {
			rep.Result = "⚠️ 迁入已中断，迁入的内容没能全部删除"
		} else {
			rep.Result = "⚠️ 迁入失败，迁入的内容没能全部删除"
		}
		lines = append(lines, "重要：请先不要启动相关容器，按下面的情况人工处理")
		for _, p := range r.rollbackIssues {
			lines = append(lines, "  · "+p)
		}
		lines = append(lines, "回滚资料："+r.rbDir+"（下次运行迁入时会提示继续删除）")
	case interrupted:
		rep.Result = "⚠️ 迁入已中断" + untouched
	default:
		rep.Result = "❌ 迁入失败" + untouched
	}
	if f := r.Screen.Failure(); f != nil && status.Message == "" {
		status.Message = f.Reason
	}
	lines = append(lines, r.d.Summary()...)
	if !success && r.downloaded {
		if _, err := os.Stat(r.bundlePath); err == nil {
			lines = append(lines, "下载的迁移包（加密）保留在 "+r.workdir+"：用同一个链接重新迁入时不用重新下载，不迁了可以删掉这个目录")
		}
	}
	rep.Lines = lines
	rep.Success = success
	if !success {
		rep.LogPath = r.Log.Path()
	}
	// 通知旧服务器：成功时它会自动结束；失败时它继续提供下载，方便重试。
	if r.m != nil || r.plan != nil {
		if err := transfer.SendStatusQuiet(ctx, r.opts.Link, status); err != nil {
			r.Log.Printf("没能通知旧服务器（可能已经关闭）：%v", err)
		}
	}
	r.Screen.PrintReport(rep)
	if success {
		return nil
	}
	return ui.ErrReported
}
