package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/lx969788249/dockhop/internal/ui"
)

// 事务：对新服务器的改动从 begin 开始，每一处都先记进事务日志（journal.go）；全部服务验证通过
// 后 commit，之后不再回滚；任何一步失败或被中断都 rollback，按日志撤销（rollback.go）。上次
// 迁入被强制结束、留下没提交的日志时，下次运行先补救。

// begin 开始事务：从这里起，对新服务器的每一处改动都先记进事务日志，失败、中断时按日志撤销。
func (r *run) begin() error {
	if err := os.RemoveAll(r.rbDir); err != nil {
		return r.Screen.StepFailed("无法清理回滚资料目录 "+r.rbDir, err.Error(), "")
	}
	j, err := openJournal(r.rbDir)
	if err != nil {
		return r.Screen.StepFailed("无法创建事务日志，为了安全没有改动任何东西", err.Error(), "检查 "+r.Home+" 是否可写")
	}
	r.j = j
	return nil
}

func (r *run) commit(ctx context.Context) error {
	s := r.Screen
	if ok, failed := r.splitGroups(); len(failed) > 0 {
		return r.commitPartial(ctx, ok, failed)
	}
	// 先把临时停下的共用数据的容器恢复；恢复不了就全部删除。
	for i := len(r.shared) - 1; i >= 0; i-- {
		e := r.shared[i]
		if err := restoreState(ctx, r.Docker, e.ID, e.State); err != nil {
			return s.StepFailed("新服务已验证，但临时停下的容器 "+e.Name+" 没能恢复", err.Error(), "")
		}
	}
	// 这是不可逆的提交点：之后不再删除。
	if err := r.j.add(Entry{Op: opCommit}); err != nil {
		return s.StepFailed(err.Error(), "", "")
	}
	r.committed = true
	entries, err := readJournal(r.rbDir)
	if err != nil {
		r.cleanupLeft = []string{"事务日志读不了（" + err.Error() + "），回滚资料没有删除"}
	} else {
		r.cleanupLeft = r.undo.cleanupCommitted(ctx, entries)
	}
	if len(r.cleanupLeft) == 0 {
		r.j.close()
		if err := os.RemoveAll(r.rbDir); err != nil {
			r.cleanupLeft = append(r.cleanupLeft, r.rbDir)
		}
	}
	r.clearDone()
	if len(r.cleanupLeft) > 0 {
		s.Warn("服务和数据都已迁入，但有些回滚资料没能删除：%s", strings.Join(r.cleanupLeft, "；"))
		s.Fail()
		return ui.ErrReported
	}
	r.noteKept(r.items)
	s.End(r.countText())
	return nil
}

// noteKept 说明迁入的容器里需要知道的情况。
func (r *run) noteKept(items []*citem) {
	for _, it := range items {
		switch {
		case it.m.OldImage != "":
			r.Screen.Info("%s 用的是旧版本的镜像（旧服务器上的 %s 已经更新过），迁入后保持原样，镜像名为 %s", it.m.Name, it.c.Config.Image, it.img)
		case it.m.Changes != nil:
			// 和在旧服务器上一样：用新镜像重建容器时，容器里的改动本来就会丢。不是迁移带来的问题，只写日志。
			r.Screen.Detail("%s 里没挂载出来的改动已一起迁入；以后用新镜像重建这个容器时，这些改动会丢失", it.m.Name)
		}
	}
}

func (r *run) rollback(ctx context.Context) {
	if r.Screen.FailedStage() == "" {
		r.Screen.Fail()
	}
	r.Screen.Println("迁入没有完成，正在删除这次迁入的内容，让新服务器回到迁入前的样子……")
	entries, err := readJournal(r.rbDir)
	if err != nil {
		r.rollbackIssues = []string{"事务日志读不了：" + err.Error()}
		return
	}
	r.rollbackIssues = r.undo.rollback(ctx, entries)
	r.rollbackDone = true
	if len(r.rollbackIssues) == 0 {
		r.j.close()
		if err := os.RemoveAll(r.rbDir); err != nil {
			r.Log.Printf("回滚资料目录没能删除：%v", err)
		}
		r.cleanHelpers(ctx)
	}
}

// recoverInterrupted 检查上次迁入有没有被强制结束、留下没删除的改动。
func (r *run) recoverInterrupted(ctx context.Context) error {
	s := r.Screen
	entries, err := readJournal(r.rbDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if errors.Is(err, ErrJournalNewer) {
		return s.StepFailed("上次迁入的事务日志是更新版本的 DockHop 写的，这个版本没法接着处理", err.Error(),
			"用一行命令运行最新版本的 DockHop，它会接着删除上次迁入的内容（回滚资料都保留在 "+r.rbDir+"）")
	}
	if err != nil {
		return s.StepFailed("上次迁入的事务日志读不了，没法自动删除上次迁入的内容", err.Error(),
			"回滚资料都保留在 "+r.rbDir+"（journal.jsonl 记着每一处改动），请人工处理后删除这个目录再重新运行")
	}
	img := r.helperImage(ctx) // 新服务器上可能一个容器都没有
	helperBin := ""
	if r.Docker.Local() {
		helperBin, _ = prepareHelper(r.Home)
	}
	r.undo.vio = &volumeIO{dc: r.Docker, image: img, helperBin: helperBin, lock: lockPath(r.Home)}
	if kept, ok := partialCommit(entries); ok {
		return r.resumePartial(ctx, entries, kept)
	}
	if committed(entries) {
		if left := r.undo.cleanupCommitted(ctx, entries); len(left) > 0 {
			s.Warn("上次迁入已经完成，但有些回滚资料没能删除：%s", strings.Join(left, "；"))
			return nil
		}
		_ = os.RemoveAll(r.rbDir)
		return nil
	}
	changes := 0
	when := ""
	for _, e := range entries {
		if e.Op == opStart {
			when = e.Time.Local().Format("01-02 15:04")
		} else {
			changes++
		}
	}
	if changes == 0 {
		return os.RemoveAll(r.rbDir)
	}
	s.Hold()
	s.Println(fmt.Sprintf("上次迁入（%s）没有完成就被结束了，新服务器上还留着它的 %d 处改动。", when, changes))
	if r.Prompt.Interactive() {
		c, err := r.Prompt.Choose([]string{"删除上次迁入的内容，回到上次迁入之前的样子（推荐）", "保留现在的样子，只删除回滚资料", "退出，我先自己检查"})
		if err != nil {
			return err
		}
		switch c {
		case 2:
			s.Warn("保留了上次迁入到一半的样子，没有删除")
			return os.RemoveAll(r.rbDir)
		case 3:
			return s.StepFailed("上次迁入留下的改动还没处理", "", "回滚资料在 "+r.rbDir+"；处理好之后重新运行")
		}
	} else {
		s.Println("没有可以输入的终端，直接删除上次迁入的内容。")
	}
	// 和这次迁入失败时的撤销一样（见 Run）：一旦开始就做完，不因为一次 Ctrl+C 停在一半（见 ui.Screen.Finish）；
	// 没能撤销的地方也一样列在结果摘要里。
	s.Finish(ctx, func(ctx context.Context) { r.rollbackIssues = r.undo.rollback(ctx, entries) })
	r.rollbackDone = true
	if len(r.rollbackIssues) > 0 {
		return s.StepFailed("上次迁入的内容没能全部删除", "", "")
	}
	s.Info("已删除上次迁入的内容，回到了上次迁入之前的样子")
	r.cleanHelpers(context.WithoutCancel(ctx))
	return os.RemoveAll(r.rbDir)
}
