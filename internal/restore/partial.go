package restore

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/ui"
)

// 只保留一部分服务：有的组没能启动、其他的已经正常运行时，问用户怎么处理（decidePartial）；选了只保留
// 正常运行的，日志里记下保留了哪几组（commitPartial），删除没启动的组、清理保留下来的组的回滚资料
// （finishPartial，哪些记录归哪边见 splitPartial 和 opRules）；做到一半程序被结束，下次运行接着做完
// （resumePartial）。

// committed 表示全部提交了：只差删除回滚资料。
func committed(entries []Entry) bool {
	for _, e := range entries {
		if e.Op == opCommit {
			return true
		}
	}
	return false
}

// partialCommit 返回只提交了一部分时保留下来的组（见 commitPartial）。
func partialCommit(entries []Entry) ([]string, bool) {
	for _, e := range entries {
		if e.Op == opPartialCommit {
			return e.Names, true
		}
	}
	return nil, false
}

// decidePartial 有的服务没能启动、其他的已经正常运行时，决定怎么处理：返回 true 表示
// 只删除没启动的、保留已经运行的。大多数人第一反应是“保留成功的”，但那样新服务器上
// 只有一半的服务，所以把代价讲清楚，默认（也是推荐）全部删除。
func (r *run) decidePartial(ok, failed []*group) (bool, error) {
	s := r.Screen
	s.Hold()
	s.Println("")
	s.Println("没能正常启动：" + labels(failed))
	s.Println("已经正常运行：" + labels(ok))
	if r.opts.KeepSucceeded {
		s.Println("按 --keep-succeeded 保留已经正常运行的服务，只删除没启动的。")
		return true, nil
	}
	if !r.Prompt.Interactive() || r.opts.Yes {
		s.Println("全部删除这次迁入的内容（想保留已经正常运行的服务，可以加上 --keep-succeeded）。")
		return false, nil
	}
	s.Println("")
	s.Println("怎么处理？")
	c, err := r.Prompt.Choose([]string{
		"全部删除（推荐）\n" +
			"     这次迁入的服务和数据全部删掉，新服务器回到迁入前的样子，环境最干净。\n" +
			"     处理好问题后，用同一个链接重新迁入即可。",
		"只删除没启动的，保留已经运行的\n" +
			"     代价：新服务器上只有一部分服务。已经运行的服务如果要用到没启动的（例如网站要连数据库），\n" +
			"     看起来在运行，实际可能用不了。没启动的服务以后还要再迁一次（用同一个链接重新迁入，\n" +
			"     会跳过已经迁入的）；如果最后决定不迁了，保留下来的服务要自己手动删除。",
	})
	if err != nil {
		return false, err
	}
	return c == 2, nil
}

// commitPartial 只删除没启动的组、提交其他组。顺序：
//  1. 恢复临时停下的、属于保留下来的组的容器（失败就整体删除，和全部成功时一样）；
//  2. 日志里记下保留下来的组：从这里开始它们就算迁入完成了；
//  3. 删除没启动的组、删除保留下来的组的回滚资料（见 finishPartial）。之后被强制结束，下次运行
//     接着做完；
//  4. 记下已经迁入的服务，重试时跳过。
func (r *migrate) commitPartial(ctx context.Context, ok, failed []*group) error {
	s := r.Screen
	failedID := map[string]bool{}
	for _, g := range failed {
		failedID[g.id] = true
	}
	for i := len(r.shared) - 1; i >= 0; i-- {
		e := r.shared[i]
		if failedID[e.Group] {
			continue
		}
		if err := restoreState(ctx, r.Docker, e.ID, e.State); err != nil {
			return &ui.Failure{Reason: "新服务已验证，但临时停下的容器 " + e.Name + " 没能恢复", Err: err}
		}
	}
	entries, err := readJournal(r.rbDir)
	if err != nil {
		return &ui.Failure{Reason: "事务日志读不了", Err: err}
	}
	var kept []string
	for _, g := range ok {
		kept = append(kept, g.id)
	}
	if err := r.j.add(Entry{Op: opPartialCommit, Names: kept}); err != nil {
		return err
	}
	r.j.close()
	r.out.committed, r.out.partial = true, true
	s.Note("删除没启动的服务")
	s.Finish(ctx, func(ctx context.Context) {
		r.out.rollbackIssues, r.out.cleanupLeft = r.finishPartial(ctx, entries, kept)
	})
	r.out.rollbackDone = true
	var items []*citem
	for _, g := range ok {
		items = append(items, g.items...)
	}
	if err := r.saveDone(items); err != nil {
		s.Warn("没能记下已经迁入的服务（%v），重新迁入时会提示它们已存在", err)
	}
	r.noteKept(items)
	if len(r.out.rollbackIssues) > 0 {
		s.Fail()
	} else {
		s.End("保留 " + labels(ok) + " · 已删除 " + labels(failed))
	}
	return errPartial
}

// finishPartial 做完只提交了一部分（保留下来的组是 kept）的迁入：没保留的组按日志撤销，保留下来的组
// 删除回滚资料（被替换的旧容器、回滚镜像、旧目录）。这两样都可以重复做，中途被强制结束时下次运行
// 从头再做一遍（见 recoverInterrupted）。撤销有没做完的，日志原样留着，下次运行接着做；只是回滚
// 资料有删不掉的，日志改写成全部提交，和全部成功时一样，下次运行接着删。
func (r *run) finishPartial(ctx context.Context, entries []Entry, kept []string) (issues, left []string) {
	drop, keep := splitPartial(entries, kept)
	issues = r.undo.rollback(ctx, drop)
	left = r.undo.cleanupCommitted(ctx, keep)
	switch {
	case len(issues) > 0:
	case len(left) > 0:
		if err := rewriteJournal(r.rbDir, append(keep, Entry{Op: opCommit, Time: time.Now()})); err != nil {
			left = append(left, "事务日志没能改写："+err.Error())
		}
	default:
		if err := os.RemoveAll(r.rbDir); err != nil {
			r.Log.Printf("回滚资料目录没能删除：%v", err)
		}
	}
	return issues, left
}

// splitPartial 把只提交了一部分的日志分成两份：要撤销的（没保留的组的改动，连同撤销时要用到的），
// 和只删除回滚资料的（保留下来的组和共用的）。每种记录归哪边写在 opRules 里。
func splitPartial(entries []Entry, kept []string) (drop, keep []Entry) {
	for _, e := range entries {
		switch opRules[e.Op].partial {
		case alwaysUndo:
			drop = append(drop, e)
		case byGroup:
			if e.Group != "" && !slices.Contains(kept, e.Group) {
				drop = append(drop, e)
			} else {
				keep = append(keep, e)
			}
		}
	}
	return drop, keep
}

// errPartial 表示只迁入了一部分（用户选择保留已经正常运行的服务）。结果已经显示过了：这一步结束时写了保留、
// 删除了哪些。
var errPartial = fmt.Errorf("只迁入了一部分：%w", ui.ErrReported)

// resumePartial 接着做完上次只提交了一部分的迁入（见 finishPartial）：用户已经选了只保留正常运行的服务，
// 删除没启动的服务时程序被结束了，接着做完，不再问。
func (r *run) resumePartial(ctx context.Context, entries []Entry, kept []string) error {
	s := r.Screen
	s.Hold()
	s.Println("上次迁入只保留了正常运行的服务，删除没启动的服务时被结束了，现在接着删除……")
	var issues, left []string
	s.Finish(ctx, func(ctx context.Context) { issues, left = r.finishPartial(ctx, entries, kept) })
	r.out.rollbackIssues, r.out.rollbackDone = issues, true // 改动过这台服务器：之后取消、失败时结果里要这样写
	r.cleanHelpers(context.WithoutCancel(ctx))
	if len(left) > 0 {
		s.Warn("上次迁入保留下来的服务有些回滚资料没能删除：%s", strings.Join(left, "；"))
	}
	if len(issues) > 0 {
		return &ui.Failure{Reason: "上次迁入时没启动的服务没能全部删除"}
	}
	s.Info("已删除上次迁入时没启动的服务")
	return nil
}
