package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/ui"
)

// 启动服务（屏幕上的第 6 步）：先建好独立网络，再一组一组地启动。每一组先按“要先存在”和
// “要先就绪”的顺序把全部容器创建好，再按“要先就绪”和“最好先就绪”的顺序启动（顺序怎么来的
// 见 order.go）；互不相关的不互相等，全部启动之后再一起确认都就绪了（ready.go）。最坏的情况
// （猜的依赖都不对）和服务器重启时一样：全部启动起来，没认出来的依赖靠应用自己重试，或者
// Docker 按重启策略重启它。一组没能启动时接着启动其他组，最后再决定怎么处理（groups.go）。

// startServices 建好网络，一组一组地启动服务。有的组没能启动时，由 decidePartial 决定只保留
// 正常运行的、还是全部删除。
func (r *migrate) startServices(ctx context.Context) error {
	// 先建好所有独立网络，再启动 Compose 项目：Compose 自动分配网段时会占用第一个空闲网段，
	// 可能正是独立网络在旧服务器上用的网段。
	byCompose := r.networkOwners()
	for _, n := range r.pkg.m.Networks {
		g := r.sc.groupByID(r.sc.groupOf["n:"+n])
		if g == nil || g.reason != "" || n == "bridge" || n == "host" || n == "none" {
			continue // 没有要迁入的容器用它
		}
		r.in("n:" + n)
		raw, err := os.ReadFile(filepath.Join(r.pkg.dataDir, "networks", n+".json"))
		if err != nil {
			r.groupFailed(g, &ui.Failure{Reason: "迁移包里缺少网络 " + n + " 的配置", Err: err})
			continue
		}
		if byCompose[n] != "" {
			continue // 交给 Compose 按项目文件创建
		}
		if err := r.ensureNetwork(ctx, n, raw); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.groupFailed(g, &ui.Failure{Err: err, Hint: "检查是否和新服务器上已有网络的网段冲突"})
		}
	}

	// 一组没能启动时，接着启动其他组，最后再决定怎么处理。
	total, n := 0, 0
	for _, it := range r.sc.items {
		if it.m.Project == "" || !r.sc.projectRestored(it.m.Project) {
			total++
		}
	}
	for _, g := range r.sc.groups {
		if g.reason != "" {
			continue
		}
		if err := r.startGroup(ctx, g, &n, total); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.groupFailed(g, err)
		}
	}
	r.in("")
	if err := r.confirmReady(ctx); err != nil {
		return err
	}
	ok, failed := r.sc.splitGroups()
	switch {
	case len(failed) == 0:
		return nil
	case len(ok) == 0:
		return ui.ErrReported
	}
	keep, err := r.decidePartial(ok, failed)
	if err != nil || !keep {
		return ui.ErrReported
	}
	return nil
}

// startGroup 启动一组服务（见本文件开头）。
func (r *migrate) startGroup(ctx context.Context, g *group, n *int, total int) error {
	units := r.units(g)
	r.logDeps(units)
	for _, u := range dependencyOrder(units, unitKey, (*unit).createAfter) {
		r.in(u.key)
		if err := r.createUnit(ctx, u); err != nil {
			return r.unitError(u, err)
		}
	}
	order := dependencyOrder(units, unitKey, (*unit).startAfter)
	if len(order) > 1 {
		var names []string
		for _, u := range order {
			names = append(names, u.label)
		}
		r.Log.Printf("启动顺序：%s", strings.Join(names, "、"))
	}
	for _, u := range order {
		if err := r.waitBefore(ctx, u); err != nil {
			return err
		}
		r.in(u.key)
		if err := r.startUnit(ctx, u, n, total); err != nil {
			return r.unitError(u, err)
		}
	}
	for _, u := range order {
		if err := r.waitUnit(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

// waitBefore 在启动 u 之前，等它用到的单位就绪：Docker 要求的必须等到；从配置里认出来的
// 等不到也接着启动（见 order.go），那个单位最后再等一次。还没启动的不等：互相用到、排不出
// 先后时会这样。
func (r *migrate) waitBefore(ctx context.Context, u *unit) error {
	for _, d := range u.deps {
		if !d.on.started || !d.ready && (!d.prefer || d.on.waited) {
			continue
		}
		err := r.waitUnit(ctx, d.on)
		switch {
		case err == nil:
		case d.ready || ctx.Err() != nil:
			return err
		default:
			r.Log.Printf("%s没有就绪（%v）；这个依赖是从配置里认出来的，可能认错了，先接着启动%s，最后再等它一次",
				d.on.label, err, u.label)
			d.on.again()
		}
	}
	return nil
}

// again 让没就绪的容器最后再有一次完整的等待时间，从那时算起，再多给 1 分钟：它可能一直在
// 等用到它的服务（猜反了），反复退出、被 Docker 自动重启，重启的间隔越拉越长，最长 1 分钟。
func (u *unit) again() {
	u.waited = true
	for _, it := range u.items {
		if !it.ready {
			it.startedAt, it.extra = time.Time{}, time.Minute
		}
	}
}

// createUnit 创建一个单位的容器（不启动）。
func (r *migrate) createUnit(ctx context.Context, u *unit) error {
	if u.project != nil {
		r.Screen.Note("创建 Compose 项目 " + u.project.Name + " 的容器")
		return r.createProject(ctx, u)
	}
	it := u.items[0]
	r.Screen.Note("创建容器 " + it.m.Name)
	if err := r.createContainer(ctx, it); err != nil {
		return err
	}
	if err := r.transplantAnonymous(ctx, it, it.newID); err != nil {
		return err
	}
	return r.applyChanges(ctx, it)
}

// unitError 写明是哪个 Compose 项目没能启动；err 没有建议、新服务器的 Compose 又是老版本时，建议换新版本。
func (r *migrate) unitError(u *unit, err error) error {
	if u.project == nil {
		return &ui.Failure{Err: err}
	}
	f := &ui.Failure{Reason: "Compose 项目 " + u.project.Name + " 启动失败", Err: err}
	if ui.HintOf(err) == "" && r.tool != nil && r.tool.Legacy() {
		f.Hint = legacyHint(r.tool)
	}
	return f
}

// startUnit 启动一个单位里原来在运行的容器，不等它们就绪。
func (r *migrate) startUnit(ctx context.Context, u *unit, n *int, total int) error {
	u.started = true
	if u.project != nil {
		return r.startProject(ctx, u)
	}
	it := u.items[0]
	*n++
	r.Screen.Count("启动容器", *n, total, it.m.Name)
	if !it.starts() {
		return nil
	}
	if err := r.Docker.StartContainer(ctx, it.newID); err != nil {
		return fmt.Errorf("容器 %s 启动失败：%v", it.m.Name, err)
	}
	it.startedAt = time.Now()
	return nil
}

// waitUnit 等一个单位里原来在运行的容器都就绪（Compose 项目按服务的依赖顺序）。
func (r *migrate) waitUnit(ctx context.Context, u *unit) error {
	if u.ready {
		return nil
	}
	items := u.items
	if u.project != nil {
		items = nil
		for _, svc := range u.serviceOrder() {
			items = append(items, replicas(u.items, svc)...)
		}
	}
	for _, it := range items {
		if err := r.readyItem(ctx, it); err != nil {
			return r.unitError(u, err)
		}
	}
	u.ready = true
	if u.project != nil && r.Test.Fails("project:"+u.project.Name) {
		return r.unitError(u, errors.New("（测试）模拟项目失败"))
	}
	if u.project == nil && r.Test.Fails("container:"+u.items[0].m.Name) {
		return &ui.Failure{Reason: "（测试）模拟容器 " + u.items[0].m.Name + " 失败"}
	}
	return nil
}

// readyItem 等一个原来在运行的容器就绪；原来暂停着的，就绪后再暂停。
func (r *migrate) readyItem(ctx context.Context, it *citem) error {
	if !it.starts() || it.ready {
		return nil
	}
	if err := r.waitReady(ctx, it); err != nil {
		return err
	}
	it.ready = true
	if it.want() == wantPaused {
		if err := r.Docker.PauseContainer(ctx, it.newID); err != nil {
			return fmt.Errorf("没能把容器 %s 恢复成暂停状态：%v", it.m.Name, err)
		}
	}
	return nil
}

// startProject 把项目里每个容器恢复成旧服务器上的状态。docker compose start 会把一个服务的
// 全部容器、连同它依赖的服务一起启动，所以只有原来全部在运行时才交给它；有停着的容器（例如
// 同一个服务一个副本在运行、一个停着）时，按依赖顺序逐个服务启动原来在运行的容器，等它们
// 就绪再启动依赖它们的服务。
func (r *migrate) startProject(ctx context.Context, u *unit) error {
	allRunning := true
	for _, it := range u.items {
		allRunning = allRunning && it.starts()
	}
	if allRunning {
		if len(u.services) == 0 {
			return nil
		}
		if err := r.tool.Exec(ctx, u.cp, r.Log.Printf, append([]string{"start"}, u.services...)...); err != nil {
			return err
		}
		now := time.Now()
		for _, it := range u.items {
			it.startedAt = now
		}
		return nil
	}
	for _, svc := range u.serviceOrder() {
		var up []*citem
		for _, it := range replicas(u.items, svc) {
			if it.starts() {
				up = append(up, it)
			}
		}
		for _, it := range up {
			if err := r.Docker.StartContainer(ctx, it.newID); err != nil {
				return fmt.Errorf("没能启动 %s：%v", it.m.Name, err)
			}
			it.startedAt = time.Now()
		}
		for _, it := range up {
			if err := r.readyItem(ctx, it); err != nil {
				return err
			}
		}
	}
	return nil
}

// groupFailed 记下一组没能启动（原因是 err），当场显示：这一步不结束，接着启动其他组。
func (r *migrate) groupFailed(g *group, err error) {
	g.reason = err.Error()
	if len(r.sc.groups) > 1 {
		r.Log.Printf("这一组没能启动：%s", g.label())
	}
	r.Screen.StepFailed(&ui.Failure{Err: err})
}
