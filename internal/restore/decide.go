package restore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/spec"
)

// 改动之前的决定：按新服务器现在的样子，决定替换、保留哪些已有服务，找出要临时停下的、和迁入
// 的数据（数据卷、目录、Compose 项目文件）共用的容器，并确认它们都能停下再恢复。判断都在这里
// 做完，事务开始之后只照着做。

// decide 是改动新服务器之前的最后一道检查，只读不改：按新服务器现在的样子决定替换、保留哪些
// 已有服务，找出要临时停下的、和迁入的数据共用的容器，并确认要停下的容器都能停下、之后再
// 原样恢复。之后的改动（加载镜像、备份已有服务、写入数据……）都在事务里，失败时全部撤销。
func (r *run) decide(ctx context.Context, chk *checked, pkg *bundleFiles) (*scope, error) {
	s := r.Screen
	dc := r.Docker
	existing, err := dc.Containers(ctx, true)
	if err != nil {
		return nil, s.StepFailed("读取容器列表失败", err.Error(), "")
	}
	byName := map[string]docker.ContainerSummary{}
	projects := map[string][]docker.ContainerSummary{}
	for _, c := range existing {
		byName[c.Name()] = c
		if p := c.Labels["com.docker.compose.project"]; p != "" {
			projects[p] = append(projects[p], c)
		}
	}
	policy := r.opts.Existing
	if policy == "" {
		policy = "replace" // 检查时已经问过
	}

	// 要迁入的内容：去掉检查时选择跳过的，--existing skip 时新服务器上已经有对应的，以及要用的名字被
	// 保留下来的已有容器占着的（见 preflight.Counterparts）。
	match := r.counterparts(chk, existing)
	// held 说明 Compose 项目 project 为什么不迁入：它里面的容器要用的名字被保留下来的已有容器占着；
	// 不是这样时为空。
	held := func(project string) string {
		for _, mc := range pkg.m.Containers {
			if h := match.Holder(mc.Name); mc.Project == project && h != "" && match.Kept(mc.Name) {
				return "新服务器上的同名容器 " + mc.Name + " 属于保留的 " + h
			}
		}
		return ""
	}
	sc := &scope{targets: map[string]bool{}, anonVolumes: map[string]bundle.Volume{}}
	var replaceProjects []string
	for _, p := range pkg.m.Projects {
		if slices.Contains(chk.d.SkipProjects, p.Name) {
			continue
		}
		if why := held(p.Name); why != "" {
			chk.d.SkipProjects = append(chk.d.SkipProjects, p.Name)
			chk.d.Skipped = append(chk.d.Skipped, preflight.Skip{Project: p.Name, Reason: why})
			continue
		}
		if what := match.ExistingFor(p.Name); what != "" {
			switch policy {
			case "skip":
				chk.d.SkipProjects = append(chk.d.SkipProjects, p.Name)
				chk.d.Skipped = append(chk.d.Skipped, preflight.Skip{Project: p.Name, Reason: "新服务器上已有" + what + "，按 --existing skip 保留"})
				continue
			case "fail":
				return nil, s.StepFailed("新服务器上已经有"+what+"（--existing fail 不允许替换），没有改动任何东西", "", "删除或改名新服务器上的这些容器，或者去掉 --existing fail")
			}
		}
		if len(projects[p.Name]) > 0 {
			replaceProjects = append(replaceProjects, p.Name)
		}
		sc.projects = append(sc.projects, p)
	}
	for _, mc := range pkg.m.Containers {
		if mc.Project != "" && slices.Contains(chk.d.SkipProjects, mc.Project) || slices.Contains(chk.d.SkipContainers, mc.Name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(pkg.dataDir, "containers", mc.Name+".json"))
		if err != nil {
			return nil, s.StepFailed("迁移包里缺少容器 "+mc.Name+" 的配置", err.Error(), "在旧服务器重新运行迁出")
		}
		var c docker.Container
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, s.StepFailed("容器 "+mc.Name+" 的配置读不懂", err.Error(), "")
		}
		// 容器启动失败时的输出可能带着这些值（例如连接串），日志和报错里都要隐藏。
		for _, v := range logging.EnvSecrets(c.Config.Env) {
			r.Redactor.AddSecret(v)
		}
		if x, ok := byName[mc.Name]; ok {
			switch h := match.Holder(mc.Name); {
			case h != "" && match.Kept(mc.Name):
				// 名字被跟着 h 保留下来的已有容器占着。Compose 项目里的容器不会到这里：整个项目上面已经跳过。
				chk.d.SkipContainers = append(chk.d.SkipContainers, mc.Name)
				chk.d.Skipped = append(chk.d.Skipped, preflight.Skip{Container: mc.Name, Reason: "新服务器上的同名容器属于保留的 " + h})
				continue
			case h != "":
				// 同名的已有容器随 h 一起替换（见 backupProject），名字空出来，不用另外备份。
			case mc.Project != "" && x.Labels["com.docker.compose.project"] == mc.Project:
				// 它属于要替换的同名 Compose 项目，随项目一起备份。
			case policy == "skip":
				chk.d.SkipContainers = append(chk.d.SkipContainers, mc.Name)
				chk.d.Skipped = append(chk.d.Skipped, preflight.Skip{Container: mc.Name, Reason: "新服务器上已有同名容器，按 --existing skip 保留"})
				continue
			case policy == "fail":
				return nil, s.StepFailed("新服务器上已经有同名容器 "+mc.Name+"（--existing fail 不允许替换），没有改动任何东西", "", "删除这个容器，或者去掉 --existing fail")
			default:
				// 它跟着这个容器（单独迁入的），或者这个容器所在的 Compose 项目：停下、改名备份，名字空出来。
				sc.targets[mc.Name] = true
			}
		}
		sc.items = append(sc.items, &citem{m: mc, raw: raw, c: &c})
	}
	for _, v := range pkg.m.Volumes {
		if v.Anonymous {
			sc.anonVolumes[v.Name] = v
		}
	}
	if len(sc.items) == 0 {
		return nil, s.StepFailed("按检查时的选择，没有要迁入的容器了", "", "")
	}
	writers, err := r.findWriters(ctx, chk, sc, existing)
	if err != nil {
		return nil, err
	}
	sc.writers, sc.replaceProjects, sc.existingProjects = writers, replaceProjects, projects
	sc.buildGroups(chk, pkg, r.Log.Printf)
	sc.seen = r.scene(chk, existing, writers)
	return sc, r.checkStoppable(ctx, sc)
}

// counterparts 说明新服务器上的已有容器迁入时会怎样：和迁入前检查是同一条规则（见
// preflight.Counterparts），检查时说会替换的，迁入时才会替换。
func (r *run) counterparts(chk *checked, existing []docker.ContainerSummary) preflight.Counterparts {
	projects := map[string]string{}
	for _, c := range existing {
		projects[c.Name()] = c.Labels["com.docker.compose.project"]
	}
	return preflight.CounterpartsOf(chk.plan.Containers, projects, &chk.d, r.opts.Existing)
}

// findWriters 找出新服务器上和要写入的数据（数据卷、挂载目录、Compose 项目的文件）共用的已有容器：
//   - 保留下来的已有服务（检查时选择了保留，或者 --existing skip）：不能覆盖它的数据。
//     检查时已经处理过，这里按新服务器现在的情况再确认一次；
//   - 其他正在运行的：要先临时停止，否则数据不一致。
//
// 读不到某个容器的配置时，没法确认它有没有在用这些数据，不能当作没有。
func (r *run) findWriters(ctx context.Context, chk *checked, sc *scope, existing []docker.ContainerSummary) ([]writer, error) {
	s := r.Screen
	match := r.counterparts(chk, existing)
	volumes, dirs := writeSet(sc.writes(chk))
	paths := realPaths(dirs)
	self, _ := os.Hostname()
	ignore := map[string]bool{}
	for _, n := range strings.Split(r.Getenv("DOCKHOP_TEST_IGNORE"), ",") {
		ignore[strings.TrimSpace(n)] = true
	}
	var writers, others []writer
	for _, c := range existing {
		name := c.Name()
		kept := match.Kept(name)
		if !kept && c.State != "running" && c.State != "paused" && c.State != "restarting" {
			continue
		}
		if match.Replaced(name) || ignore[name] || (len(self) >= 12 && strings.HasPrefix(c.ID, self)) {
			continue
		}
		full, _, err := r.Docker.InspectContainer(ctx, c.ID)
		switch {
		case docker.IsNotFound(err):
			continue // 刚好被删掉了
		case err != nil:
			return nil, s.StepFailed("读不到新服务器上的容器 "+name+"，没法确认它有没有在用要写入的数据（还没有改动已有的服务和数据）", err.Error(), "确认 Docker 运行正常后重新运行")
		case !spec.UsesData(full, volumes, paths):
			others = append(others, writer{summary: c, full: full})
			continue
		case kept:
			return nil, s.StepFailed("保留的已有容器 "+name+" 和要迁入的容器共用数据，迁入会覆盖它正在用的数据（还没有改动已有的服务和数据）", "",
				"重新运行迁入，检查时把共用数据的容器一起跳过，或者替换这个已有的容器")
		}
		writers = append(writers, writer{summary: c, full: full})
	}
	return withBorrowers(writers, others), nil
}

// withBorrowers 把借用要停下的容器的网络、进程空间的已有容器（例如 VPN 的边车）也加进来：被借用的一停，
// 它们就断了，重新启动后也不会自己接回去（迁出时一样）。借用的排在被借用的前面：先停借用的，恢复时
// 倒过来、后启动。
func withBorrowers(writers, others []writer) []writer {
	stopping := map[string]bool{}
	for _, w := range writers {
		stopping[w.full.ID] = true
	}
	for changed := true; changed; {
		changed = false
		for _, o := range others {
			if stopping[o.full.ID] {
				continue
			}
			for _, ref := range spec.Dependencies(o.full) {
				if slices.ContainsFunc(writers, func(w writer) bool { return refersTo(ref, w) }) {
					writers = append([]writer{o}, writers...)
					stopping[o.full.ID] = true
					changed = true
					break
				}
			}
		}
	}
	return writers
}

// refersTo 表示容器配置里写的 ref（名字、ID 或者 ID 的前 12 位以上）指的是不是 w。
func refersTo(ref string, w writer) bool {
	ref = strings.TrimPrefix(ref, "/")
	return ref == w.summary.Name() || ref == w.full.ID || len(ref) >= 12 && strings.HasPrefix(w.full.ID, ref)
}

// scene 是新服务器上和这次迁入有关的已有容器：要替换的（名字、ID），要临时停下的、和迁入的
// 数据共用的（名字、ID、状态）。decide 时记下，改动之前再核对一次（见 recheck）。
func (r *run) scene(chk *checked, existing []docker.ContainerSummary, writers []writer) []string {
	match := r.counterparts(chk, existing)
	var out []string
	for _, c := range existing {
		if match.Replaced(c.Name()) {
			out = append(out, c.Name()+" "+c.ID)
		}
	}
	for _, w := range writers {
		state := stateOf(w.full)
		if state == "restarting" {
			state = "running" // 一直在重启的容器两种状态来回变，恢复时也一样处理（见 restoreState）
		}
		out = append(out, w.summary.Name()+" "+w.summary.ID+" "+state)
	}
	slices.Sort(out)
	return out
}

// recheck 在改动已有的服务之前，再看一次新服务器上和这次迁入有关的容器。decide 之后加载镜像
// 可能花了好一会儿，这期间有容器新建、启动、停下，或者被重建（例如被 Watchtower 更新了），再按
// decide 时的样子停容器、备份就不对了：新启动的共用数据的容器没停下，数据写进去时它还在用。
// 有变化就停下，重新运行会按新服务器现在的样子重新检查。
func (r *migrate) recheck(ctx context.Context) error {
	s := r.Screen
	existing, err := r.Docker.Containers(ctx, true)
	if err != nil {
		return s.StepFailed("读取容器列表失败", err.Error(), "")
	}
	writers, err := r.findWriters(ctx, r.chk, r.sc, existing)
	if err != nil {
		return err
	}
	now := r.scene(r.chk, existing, writers)
	var changed []string
	for _, e := range append(slices.Clone(r.sc.seen), now...) {
		if slices.Contains(r.sc.seen, e) != slices.Contains(now, e) {
			changed = append(changed, strings.Fields(e)[0])
		}
	}
	if len(changed) > 0 {
		slices.Sort(changed)
		return s.StepFailed("新服务器上的容器 "+strings.Join(slices.Compact(changed), "、")+" 在检查之后有了变化，为了安全没有动已有的服务和数据", "",
			"重新运行迁入，会按新服务器现在的样子重新检查")
	}
	return nil
}

// checkStoppable 确认迁入时要停下的已有容器都能停下、之后再原样恢复（规则见
// spec.StopRiskOf）：共用数据的容器，以及要被替换的同名容器和同名 Compose 项目的容器。
func (r *run) checkStoppable(ctx context.Context, sc *scope) error {
	type stopping struct {
		c   *docker.Container
		why string
	}
	var list []stopping
	for _, w := range sc.writers {
		list = append(list, stopping{w.full, "它和迁入的服务共用数据"})
	}
	var ids []string
	for _, it := range sc.items {
		if sc.targets[it.m.Name] {
			ids = append(ids, it.m.Name)
		}
	}
	for _, p := range sc.replaceProjects {
		for _, c := range sc.existingProjects[p] {
			ids = append(ids, c.ID)
		}
	}
	for _, id := range ids {
		c, _, err := r.Docker.InspectContainer(ctx, id)
		if err != nil {
			return r.Screen.StepFailed("读不到新服务器上要替换的容器 "+id+"（还没有改动任何东西）", err.Error(), "确认 Docker 运行正常后重新运行")
		}
		list = append(list, stopping{c, "要被迁入的同名服务替换"})
	}
	for _, x := range list {
		name := x.c.ShortName()
		switch spec.StopRiskOf(x.c) {
		case spec.StopRemoves:
			return r.Screen.StepFailed("新服务器上的容器 "+name+" 是用 --rm 启动的：迁入时要先停下它（"+x.why+
				"），可 Docker 一停止就会把它连同匿名卷和容器里的改动一起删掉，迁入失败时也放不回来（还没有改动任何东西）", "",
				"先把它重新创建成不带 --rm 的容器，再重新运行迁入")
		case spec.StopReplaced:
			svc := x.c.Config.Labels["com.docker.swarm.service.name"]
			return r.Screen.StepFailed("新服务器上的容器 "+name+" 属于 Swarm 服务 "+svc+"：迁入时要先停下它（"+x.why+
				"），可它一停就会被 Swarm 换成新的，新的照样在用这些数据（还没有改动任何东西）", "",
				"先执行 docker service scale "+svc+"=0，迁入完成后再改回原来的个数")
		}
	}
	return nil
}

// writes 是这次迁入要写到新服务器上的东西，和迁入前检查按同一份迁移计划算（见 preflight.WritesOf）。
func (sc *scope) writes(chk *checked) preflight.Writes {
	names := map[string]bool{}
	for _, it := range sc.items {
		names[it.m.Name] = true
	}
	var migrating []bundle.PlanContainer
	for _, c := range chk.plan.Containers {
		if names[c.Name] {
			migrating = append(migrating, c)
		}
	}
	return preflight.WritesOf(chk.plan, &chk.d, migrating)
}

// dataTargets 是 w 里要写入数据的有名字的数据卷（匿名卷随容器新建）和目录（系统路径只重建挂载、
// 不覆盖，见 restoreData）。
func dataTargets(w preflight.Writes) (map[string]bool, []string) {
	vols := map[string]bool{}
	for _, v := range w.Volumes {
		if !spec.IsAnonymousVolume(v) {
			vols[v] = true
		}
	}
	var binds []string
	for _, b := range w.Binds {
		if !spec.IsSystemPath(b) {
			binds = append(binds, b)
		}
	}
	return vols, binds
}

// writeTarget 是迁入要写到宿主机上的一处，key 是它在分组里的键：挂载目录是 b:路径，
// Compose 项目的目录和项目目录外的文件是 p:项目名。
type writeTarget struct{ key, path string }

// writeSet 是 w 里要写入的全部地方：数据卷，以及宿主机上的挂载目录、Compose 项目目录和项目目录外的
// 文件。新服务器上和它们共用数据的已有容器要先停下（保留的服务则不能迁入），分组时也和写入它们的服务
// 放在一组。
func writeSet(w preflight.Writes) (map[string]bool, []writeTarget) {
	vols, binds := dataTargets(w)
	var dirs []writeTarget
	for _, b := range binds {
		dirs = append(dirs, writeTarget{"b:" + b, b})
	}
	for _, p := range w.Projects {
		for _, path := range append([]string{p.WorkingDir}, p.Outside...) {
			if !spec.IsSystemPath(path) {
				dirs = append(dirs, writeTarget{"p:" + p.Name, path})
			}
		}
	}
	return vols, dirs
}

// realPaths 是这些要写的地方的实际位置（解析软链接之后），和容器挂载的路径比较用（见 spec.UsesData）。
func realPaths(dirs []writeTarget) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = host.RealPath(d.path)
	}
	return out
}
