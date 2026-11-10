package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 迁入前检查：从旧服务器取来迁移计划，按它检查新服务器能不能迁入。这时还没下载、也没改动
// 任何东西；迁移计划是检查和核对下载文件的依据，没有它不能迁入。

func (r *run) client() *transfer.Client {
	timeout := 15 * time.Second
	if v := r.Getenv("DOCKHOP_TEST_CONNECT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	return transfer.NewClient(r.opts.Link, timeout, r.Log.Printf)
}

func (r *run) fetchPlan(ctx context.Context) error {
	s := r.Screen
	s.Note("连接旧服务器 " + r.opts.Link.Host)
	raw, err := r.client().FetchPlan(ctx)
	if errors.Is(err, transfer.ErrNoPlan) {
		// 迁移计划是迁入前检查和核对下载文件的依据，没有它不能迁入。
		return s.StepFailed("旧服务器没有提供迁移计划，没法做迁入前检查（还没有下载或改动任何东西）", "",
			"在旧服务器上重新运行迁出")
	}
	if err != nil {
		var ce *transfer.ConnError
		if errors.As(err, &ce) {
			return s.StepFailed(ce.Reason, "", ce.Hint)
		}
		return s.StepFailed("连接旧服务器失败", err.Error(), "")
	}
	p, err := bundle.DecryptPlan(bytes.NewReader(raw), r.opts.Link)
	if err != nil {
		return s.StepFailed("迁移计划解不开："+err.Error(), "", "确认用的是旧服务器这次显示的完整链接")
	}
	r.plan = p
	return nil
}

// check 是迁入前检查，返回这一步的摘要。
func (r *run) check(ctx context.Context) (string, error) {
	s := r.Screen
	r.skipDone(ctx)
	runner := &preflight.Runner{Screen: s, Prompt: r.Prompt, Auto: r.opts.Yes,
		Collect: func() (*preflight.Target, error) {
			v := ""
			if r.tool != nil {
				v = r.tool.Version
			}
			return preflight.Collect(ctx, r.Docker, v)
		}}
	res, err := runner.Run(r.plan, &r.d, preflight.Options{Existing: r.opts.Existing, BaseDir: r.workBase(), Home: r.Home, LocalWork: localWork})
	if ui.Canceled(err) {
		return "", err
	}
	if err != nil {
		var p *preflight.Problem
		if errors.As(err, &p) {
			return "", s.StepFailed(p.Reason, "", p.Hint)
		}
		return "", s.StepFailed(err.Error(), "", "")
	}
	b, _ := json.Marshal(r.d)
	r.Log.Printf("检查时的选择：%s", b)
	summary := "没有发现问题"
	if runner.Raised() {
		summary = "检查完成"
	}
	if res.NeedBytes > 0 {
		summary += " · 需要约 " + ui.HumanBytes(res.NeedBytes) + " 磁盘空间"
	}
	return summary, nil
}

// localWork 列出要写的这些东西里只能在 Docker 所在的机器上做的事，Docker 不在本机时迁入前检查
// 据此停下：往宿主机上写目录（restoreBind）、放 Compose 项目文件（installProject），以及写之前要先
// 清空的数据卷（见 mustClear）。只写进新建的有名字的卷不用清空，Docker 在别的机器上也能做。
func localWork(w preflight.Writes, t *preflight.Target) []string {
	var what []string
	if len(w.Binds) > 0 {
		what = append(what, "放目录")
	}
	if len(w.Projects) > 0 {
		what = append(what, "放 Compose 项目文件")
	}
	for _, v := range w.Volumes {
		if mustClear(v, t.Volumes[v] != nil) {
			what = append(what, "替换已有的数据卷")
			break
		}
	}
	return what
}
