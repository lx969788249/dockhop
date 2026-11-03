package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 启动一组服务：组里的每个 Compose 项目、每个独立容器是一个“启动单位”。单位之间的依赖分三种，
// 强弱不同，各管各的事：
//
//   - 要先存在（exist）：被借用的 Compose 网络由另一个项目创建，或者 --volumes-from 另一个容器。
//     只影响创建的先后。
//   - 要先就绪（ready）：借用另一个容器的网络、进程、IPC 命名空间，或者 --link 它。Docker 要求
//     被依赖的先在运行，所以它先启动、就绪了才启动用到它的；它起不来，用到它的也不启动。
//   - 最好先就绪（prefer）：从配置里认出来的，容器的环境变量或启动命令里写着同一个网络上另一个
//     容器的名字或网络别名。例如 1Panel 装的 Halo 和 PostgreSQL 是两个 Compose 项目，Halo 的
//     启动参数里写着数据库的服务名（r2dbc:pool:postgresql://postgresql:5432/halo）。被认出来的
//     先启动，等它就绪了再启动用到它的；可这是猜的，可能认错（某个词碰巧和别的容器同名，甚至
//     方向反了），所以等不到时不拦着：接着启动后面的，最后再给它一次完整的等待时间。
//
// 先按“要先存在”和“要先就绪”的顺序把全部容器创建好，再按“要先就绪”和“最好先就绪”的顺序
// 启动；互不相关的不互相等，全部启动之后再一起确认都就绪了。最坏的情况（猜的依赖都不对）和
// 服务器重启时一样：全部启动起来，没认出来的依赖靠应用自己重试，或者 Docker 按重启策略重启它。

// unit 是一起启动的一个 Compose 项目或者一个独立容器。
type unit struct {
	key     string          // 和分组用的键一样：p:项目名、c:容器名
	label   string          // 写进日志的名字
	project *bundle.Project // 独立容器为 nil
	items   []*citem
	deps    []*unitDep
	started bool
	ready   bool
	waited  bool // 作为“最好先就绪”的依赖等过它、没等到：后面不再为它等，最后再等一次

	// Compose 项目创建时算出来的，启动和等待时还要用
	cp         compose.Project
	services   []string
	configured map[string]compose.Service
}

// unitDep 是一个单位用到另一个单位（见本文件开头）；why 写进日志。
type unitDep struct {
	on                   *unit
	exist, ready, prefer bool
	why                  []string
}

func unitKey(u *unit) string { return u.key }

// createAfter 是创建它之前要先创建好的单位。
func (u *unit) createAfter() []string {
	var out []string
	for _, d := range u.deps {
		if d.exist || d.ready {
			out = append(out, d.on.key)
		}
	}
	return out
}

// startAfter 是启动它之前要（最好）先就绪的单位。
func (u *unit) startAfter() []string {
	var out []string
	for _, d := range u.deps {
		if d.ready || d.prefer {
			out = append(out, d.on.key)
		}
	}
	return out
}

// serviceOrder 是 Compose 项目里的服务按 depends_on 排好的顺序。
func (u *unit) serviceOrder() []string {
	return dependencyOrder(u.services, func(svc string) string { return svc },
		func(svc string) []string { return u.configured[svc].Needs() })
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

// units 列出一组服务里的启动单位，并找出它们之间的依赖。
func (r *run) units(g *group) []*unit {
	var out []*unit
	for i := range g.projects {
		p := &g.projects[i]
		u := &unit{key: "p:" + p.Name, label: "项目 " + p.Name, project: p}
		for _, it := range r.items {
			if it.m.Project == p.Name {
				u.items = append(u.items, it)
			}
		}
		out = append(out, u)
	}
	for _, it := range g.items {
		if it.m.Project == "" || !r.projectRestored(it.m.Project) {
			out = append(out, &unit{key: "c:" + it.m.Name, label: "容器 " + it.m.Name, items: []*citem{it}})
		}
	}
	byKey := map[string]*unit{}
	owner := map[string]*unit{}
	for _, u := range out {
		byKey[u.key] = u
		for _, it := range u.items {
			owner[it.m.Name] = u
		}
	}
	networkOwner := r.networkOwners()
	for _, u := range out {
		dep := func(on *unit) *unitDep {
			if on == nil || on == u {
				return nil
			}
			for _, d := range u.deps {
				if d.on == on {
					return d
				}
			}
			d := &unitDep{on: on}
			u.deps = append(u.deps, d)
			return d
		}
		for _, it := range u.items {
			for _, x := range spec.Deps(it.c) {
				name := x.Ref
				if n := r.nameOfOld(x.Ref); n != "" {
					name = n
				}
				if d := dep(owner[name]); d != nil {
					how := "--volumes-from"
					if x.Running {
						d.ready, how = true, "借用网络（或 --link）"
					}
					d.exist = true
					d.why = append(d.why, it.m.Name+" 用"+how+"连着 "+name)
				}
			}
			for n := range it.c.NetworkSettings.Networks {
				if proj := networkOwner[n]; proj != "" {
					if d := dep(byKey["p:"+proj]); d != nil {
						d.exist = true
						d.why = append(d.why, it.m.Name+" 接在 Compose 项目 "+proj+" 建的网络 "+n+" 上")
					}
				}
			}
		}
		for _, v := range out {
			if v == u {
				continue
			}
		next:
			for _, it := range u.items {
				for _, other := range v.items {
					if why := mentions(it.c, other.c); why != "" {
						d := dep(v)
						d.prefer = true
						d.why = append(d.why, it.m.Name+" 的"+why)
						break next
					}
				}
			}
		}
	}
	return out
}

// networkOwners 是由迁入的 Compose 项目创建的网络 → 项目名：这些网络交给 Compose 按项目文件
// 创建（见 startServices），用到它们的其他单位要等这个项目创建好。
func (r *run) networkOwners() map[string]string {
	out := map[string]string{}
	for _, n := range r.m.Networks {
		raw, err := os.ReadFile(filepath.Join(r.dataDir, "networks", n+".json"))
		if err != nil {
			continue
		}
		var meta struct {
			Labels map[string]string `json:"Labels"`
		}
		_ = json.Unmarshal(raw, &meta)
		if proj := meta.Labels["com.docker.compose.project"]; proj != "" && r.projectRestored(proj) &&
			meta.Labels["com.docker.compose.network"] != "" {
			out[n] = proj
		}
	}
	return out
}

// mentions 说明容器 a 的配置里写没写着容器 b 的名字：b 接在 a 所在的某个网络上，在那里能按
// 这个名字找到它（容器名、网络别名），而 a 的某个环境变量的值或启动命令里有一个词正好是它
// （不分大小写）。返回写进日志的理由，只写变量名，不写值（值里可能有密码）；没写着时返回空。
func mentions(a, b *docker.Container) string {
	names := map[string]bool{}
	for n, ep := range b.NetworkSettings.Networks {
		if _, shared := a.NetworkSettings.Networks[n]; !shared || n == "bridge" || n == "host" || n == "none" {
			continue
		}
		for _, name := range append(append([]string{b.ShortName()}, ep.Aliases...), ep.DNSNames...) {
			if name != "" && !hexID(name) {
				names[strings.ToLower(name)] = true
			}
		}
	}
	if len(names) == 0 {
		return ""
	}
	for _, kv := range a.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		if w := wordIn(v, names); w != "" {
			return fmt.Sprintf("环境变量 %s 里写着 %s", k, w)
		}
	}
	for _, arg := range append(append([]string{}, a.Config.Entrypoint...), a.Config.Cmd...) {
		if w := wordIn(arg, names); w != "" {
			return "启动命令里写着 " + w
		}
	}
	return ""
}

// wordIn 把 s 按主机名里不会出现的字符切成词，返回第一个在 names 里的词。
func wordIn(s string, names map[string]bool) string {
	for _, w := range strings.FieldsFunc(s, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.')
	}) {
		if names[strings.ToLower(w)] {
			return w
		}
	}
	return ""
}

// hexID 表示是容器 ID（或它的前 12 位），不是起的名字。
func hexID(s string) bool {
	if len(s) != 12 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// startGroup 启动一组服务（见本文件开头）。
func (r *run) startGroup(ctx context.Context, g *group, n *int, total int) error {
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
// 等不到也接着启动（见本文件开头），那个单位最后再等一次。还没启动的不等：互相用到、排不出
// 先后时会这样。
func (r *run) waitBefore(ctx context.Context, u *unit) error {
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

// createUnit 创建一个单位的容器（不启动）。
func (r *run) createUnit(ctx context.Context, u *unit) error {
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

// logDeps 把单位之间的依赖和原因写进日志。
func (r *run) logDeps(units []*unit) {
	for _, u := range units {
		for _, d := range u.deps {
			kind := "要等它先建好"
			switch {
			case d.ready:
				kind = "要等它先就绪"
			case d.prefer && !d.exist:
				kind = "最好等它先就绪"
			case d.prefer:
				kind = "要等它先建好，最好也先就绪"
			}
			r.Log.Printf("%s 用到%s，%s：%s", u.label, d.on.label, kind, strings.Join(d.why, "；"))
		}
	}
}

// unitError 写明是哪个 Compose 项目没能启动，带上建议。
func (r *run) unitError(u *unit, err error) error {
	reason, hint := err.Error(), groupHint(err)
	if u.project == nil {
		return &startError{reason, hint}
	}
	if hint == "" && r.tool != nil && r.tool.Legacy() {
		hint = r.legacyHint()
	}
	return &startError{"Compose 项目 " + u.project.Name + " 启动失败：" + reason, hint}
}

// startUnit 启动一个单位里原来在运行的容器，不等它们就绪。
func (r *run) startUnit(ctx context.Context, u *unit, n *int, total int) error {
	u.started = true
	if u.project != nil {
		return r.startProject(ctx, u)
	}
	it := u.items[0]
	*n++
	r.Screen.Count("启动容器", *n, total, it.m.Name)
	if !it.running() {
		return nil
	}
	if err := r.Docker.StartContainer(ctx, it.newID); err != nil {
		return fmt.Errorf("容器 %s 启动失败：%v", it.m.Name, err)
	}
	it.startedAt = time.Now()
	return nil
}

// waitUnit 等一个单位里原来在运行的容器都就绪（Compose 项目按服务的依赖顺序）。
func (r *run) waitUnit(ctx context.Context, u *unit) error {
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
	if u.project != nil && r.testFail("project:"+u.project.Name) {
		return r.unitError(u, errors.New("（测试）模拟项目失败"))
	}
	if u.project == nil && r.testFail("container:"+u.items[0].m.Name) {
		return &startError{"（测试）模拟容器 " + u.items[0].m.Name + " 失败", ""}
	}
	return nil
}

// readyItem 等一个原来在运行的容器就绪；原来暂停着的，就绪后再暂停。
func (r *run) readyItem(ctx context.Context, it *citem) error {
	if !it.running() || it.ready {
		return nil
	}
	if err := r.waitReady(ctx, it); err != nil {
		return err
	}
	it.ready = true
	if it.c.State.Paused {
		if err := r.Docker.PauseContainer(ctx, it.newID); err != nil {
			return fmt.Errorf("没能把容器 %s 恢复成暂停状态：%v", it.m.Name, err)
		}
	}
	return nil
}

// createProject 用 Compose 创建项目的容器（不启动），放回匿名卷的数据和容器里的改动。
func (r *run) createProject(ctx context.Context, u *unit) error {
	if r.tool == nil {
		return errors.New("新服务器没有安装 docker compose")
	}
	p := u.project
	members := u.items
	// -f 按旧服务器上的原样写法传（在项目目录里运行），容器标签里记的配置文件才和原来一样。
	files := p.ConfigArgs
	if len(files) == 0 {
		files = p.ConfigFiles
	}
	cp := compose.Project{Name: p.Name, WorkingDir: p.WorkingDir, Files: append([]string(nil), files...)}
	// 旧版本镜像、改名的 digest 镜像、删掉了文件的镜像：用一个临时的覆盖文件按服务指定镜像，
	// 不改用户自己的文件。
	override := map[string]map[string]string{}
	for _, svc := range uniqueServices(members) {
		reps := replicas(members, svc)
		img, err := r.serviceImage(ctx, p.Name, svc, reps)
		if err != nil {
			return err
		}
		if img != reps[0].c.Config.Image {
			override[svc] = map[string]string{"image": img}
		}
	}
	if len(override) > 0 {
		path := filepath.Join(r.workdir, "dockhop-override-"+p.Name+".yml")
		b, _ := json.Marshal(map[string]any{"services": override}) // JSON 也是合法的 YAML
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return err
		}
		cp.Files = append(cp.Files, path)
	}
	nets, _ := r.Docker.Networks(ctx)
	var before []string
	for _, n := range nets {
		if full, err := r.Docker.InspectNetwork(ctx, n.ID); err == nil && full.Labels["com.docker.compose.project"] == p.Name {
			before = append(before, full.Name)
		}
	}
	if err := r.j.add(Entry{Op: opComposeUp, Name: p.Name, Names: before}); err != nil {
		return err
	}
	// 一个服务可能有多个副本（deploy.replicas、--scale）：按旧服务器上的个数创建，
	// 再按副本编号一一对应，每个副本的匿名卷和容器里的改动放进对应的新容器。
	// 只在个数和配置文件里写的不一样时才加 --scale：有的 Compose 版本（例如 2.17）会把它
	// 算进记在容器上的配置摘要，之后用户自己 docker compose up 时就会重建容器。
	services := uniqueServices(members)
	args := append([]string{"up", "--no-start"}, r.tool.PullMissing(ctx)...)
	configured, err := r.tool.Services(ctx, cp)
	if err != nil {
		r.Log.Printf("读不出 Compose 项目 %s 配置里的副本个数：%v", p.Name, err)
	}
	for _, svc := range services {
		want, have := len(replicas(members, svc)), 1
		if c, ok := configured[svc]; ok {
			have = c.Replicas()
		}
		if want != have {
			args = append(args, "--scale", fmt.Sprintf("%s=%d", svc, want))
		}
	}
	if err := r.tool.Exec(ctx, cp, r.Log.Printf, append(args, services...)...); err != nil {
		return err
	}
	list, err := r.Docker.ContainersByLabel(ctx, "com.docker.compose.project="+p.Name)
	if err != nil {
		return err
	}
	for _, svc := range services {
		old := replicas(members, svc)
		var created []docker.ContainerSummary
		for _, c := range list {
			if c.Labels["com.docker.compose.service"] == svc {
				created = append(created, c)
			}
		}
		sort.SliceStable(created, func(i, j int) bool {
			return replicaNumber(created[i].Labels) < replicaNumber(created[j].Labels)
		})
		if len(created) != len(old) {
			return fmt.Errorf("服务 %s 在旧服务器上有 %d 个容器，新服务器上创建出了 %d 个", svc, len(old), len(created))
		}
		for i, it := range old {
			it.newID = created[i].ID
		}
	}
	for _, it := range members {
		if err := r.transplantAnonymous(ctx, it, it.newID); err != nil {
			return err
		}
		if err := r.applyChanges(ctx, it); err != nil {
			return err
		}
	}
	u.cp, u.services, u.configured = cp, services, configured
	return nil
}

// startProject 把项目里每个容器恢复成旧服务器上的状态。docker compose start 会把一个服务的
// 全部容器、连同它依赖的服务一起启动，所以只有原来全部在运行时才交给它；有停着的容器（例如
// 同一个服务一个副本在运行、一个停着）时，按依赖顺序逐个服务启动原来在运行的容器，等它们
// 就绪再启动依赖它们的服务。
func (r *run) startProject(ctx context.Context, u *unit) error {
	allRunning := true
	for _, it := range u.items {
		allRunning = allRunning && it.running()
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
			if it.running() {
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

func (r *run) startServices(ctx context.Context) error {
	// 先建好所有独立网络，再启动 Compose 项目：Compose 自动分配网段时会占用第一个空闲网段，
	// 可能正是独立网络在旧服务器上用的网段。
	for _, n := range r.m.Networks {
		g := r.groupByID(r.groupOf["n:"+n])
		if g == nil || g.reason != "" || n == "bridge" || n == "host" || n == "none" {
			continue // 没有要迁入的容器用它
		}
		r.in("n:" + n)
		raw, err := os.ReadFile(filepath.Join(r.dataDir, "networks", n+".json"))
		if err != nil {
			r.groupFailed(g, "迁移包里缺少网络 "+n+" 的配置："+err.Error(), "")
			continue
		}
		var meta struct {
			Labels map[string]string `json:"Labels"`
		}
		_ = json.Unmarshal(raw, &meta)
		if proj := meta.Labels["com.docker.compose.project"]; proj != "" && r.projectRestored(proj) && meta.Labels["com.docker.compose.network"] != "" {
			continue // 交给 Compose 按项目文件创建
		}
		if err := r.ensureNetwork(ctx, n, raw); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.groupFailed(g, err.Error(), "检查是否和新服务器上已有网络的网段冲突")
		}
	}

	// 一组没能启动时，接着启动其他组，最后再决定怎么处理。
	total, n := 0, 0
	for _, it := range r.items {
		if it.m.Project == "" || !r.projectRestored(it.m.Project) {
			total++
		}
	}
	for _, g := range r.groups {
		if g.reason != "" {
			continue
		}
		if err := r.startGroup(ctx, g, &n, total); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.groupFailed(g, err.Error(), groupHint(err))
		}
	}
	r.in("")
	ok, failed := r.splitGroups()
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

// startError 是一组服务没能启动的原因和建议。
type startError struct{ reason, hint string }

func (e *startError) Error() string { return e.reason }

func groupHint(err error) string {
	var se *startError
	if errors.As(err, &se) {
		return se.hint
	}
	return ""
}

// groupFailed 记下一组没能启动，并显示原因。
func (r *run) groupFailed(g *group, reason, hint string) {
	g.reason = reason
	if len(r.groups) > 1 {
		r.Log.Printf("这一组没能启动：%s", g.label())
	}
	_ = r.Screen.StepFailed(reason, "", hint)
}

// replicas 是服务 svc 在旧服务器上的容器，按副本编号排好。
func replicas(items []*citem, svc string) []*citem {
	var out []*citem
	for _, it := range items {
		if it.m.Service == svc {
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return replicaNumber(out[i].c.Config.Labels) < replicaNumber(out[j].c.Config.Labels)
	})
	return out
}

// replicaNumber 是 Compose 给容器记的副本编号（com.docker.compose.container-number）。
func replicaNumber(labels map[string]string) int {
	n, _ := strconv.Atoi(labels["com.docker.compose.container-number"])
	return n
}

func uniqueServices(items []*citem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.m.Service)
	}
	return uniq(out)
}

func uniq(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range list {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
