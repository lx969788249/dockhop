package restore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lx969788249/dockhop/internal/docker"
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
func (r *run) decide(ctx context.Context) error {
	s := r.Screen
	dc := r.Docker
	existing, err := dc.Containers(ctx, true)
	if err != nil {
		return s.StepFailed("读取容器列表失败", err.Error(), "")
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

	// 要迁入的内容：去掉检查时选择跳过的，以及 --existing skip 时已经有的。
	r.targets = map[string]bool{}
	var replaceProjects []string
	for _, p := range r.m.Projects {
		if slices.Contains(r.d.SkipProjects, p.Name) {
			continue
		}
		if len(projects[p.Name]) > 0 {
			switch policy {
			case "skip":
				r.d.SkipProjects = append(r.d.SkipProjects, p.Name)
				r.d.Skipped = append(r.d.Skipped, preflight.Skip{Project: p.Name, Reason: "新服务器上已有同名项目，按 --existing skip 保留"})
				continue
			case "fail":
				return s.StepFailed("新服务器上已经有同名的 Compose 项目 "+p.Name+"（--existing fail 不允许替换），没有改动任何东西", "", "删除这个项目的容器，或者去掉 --existing fail")
			}
			replaceProjects = append(replaceProjects, p.Name)
		}
		r.projects = append(r.projects, p)
	}
	for _, mc := range r.m.Containers {
		if mc.Project != "" && slices.Contains(r.d.SkipProjects, mc.Project) || slices.Contains(r.d.SkipContainers, mc.Name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r.dataDir, "containers", mc.Name+".json"))
		if err != nil {
			return s.StepFailed("迁移包里缺少容器 "+mc.Name+" 的配置", err.Error(), "在旧服务器重新运行迁出")
		}
		var c docker.Container
		if err := json.Unmarshal(raw, &c); err != nil {
			return s.StepFailed("容器 "+mc.Name+" 的配置读不懂", err.Error(), "")
		}
		// 容器启动失败时的输出可能带着这些值（例如连接串），日志和报错里都要隐藏。
		for _, v := range logging.EnvSecrets(c.Config.Env) {
			r.Redactor.AddSecret(v)
		}
		inProject := mc.Project != "" && r.projectRestored(mc.Project)
		if !inProject {
			if _, ok := byName[mc.Name]; ok {
				switch policy {
				case "skip":
					r.d.SkipContainers = append(r.d.SkipContainers, mc.Name)
					r.d.Skipped = append(r.d.Skipped, preflight.Skip{Container: mc.Name, Reason: "新服务器上已有同名容器，按 --existing skip 保留"})
					continue
				case "fail":
					return s.StepFailed("新服务器上已经有同名容器 "+mc.Name+"（--existing fail 不允许替换），没有改动任何东西", "", "删除这个容器，或者去掉 --existing fail")
				}
				r.targets[mc.Name] = true
			}
		}
		r.items = append(r.items, &citem{m: mc, raw: raw, c: &c})
	}
	for _, v := range r.m.Volumes {
		if v.Anonymous {
			r.anonVolumes[v.Name] = v
		}
	}
	if len(r.items) == 0 {
		return s.StepFailed("按检查时的选择，没有要迁入的容器了", "", "")
	}

	// 新服务器上和要写入的数据（数据卷、挂载目录、Compose 项目的文件）共用的容器：
	//   - 保留下来的已有服务（检查时选择了保留，或者 --existing skip）：不能覆盖它的数据。
	//     检查时已经处理过，这里按新服务器现在的情况再确认一次；
	//   - 其他正在运行的：要先临时停止，否则数据不一致。
	// 读不到某个容器的配置时，没法确认它有没有在用这些数据，不能当作没有。
	volumes, dirs := r.writeSet()
	self, _ := os.Hostname()
	ignore := map[string]bool{}
	for _, n := range strings.Split(r.Getenv("DOCKHOP_TEST_IGNORE"), ",") {
		ignore[strings.TrimSpace(n)] = true
	}
	var writers []writer
	for _, c := range existing {
		name, project := c.Name(), c.Labels["com.docker.compose.project"]
		kept := slices.Contains(r.d.SkipContainers, name) || slices.Contains(r.d.SkipProjects, project)
		if !kept && c.State != "running" && c.State != "paused" && c.State != "restarting" {
			continue
		}
		if r.targets[name] || slices.Contains(replaceProjects, project) || ignore[name] || (len(self) >= 12 && strings.HasPrefix(c.ID, self)) {
			continue
		}
		full, _, err := dc.InspectContainer(ctx, c.ID)
		switch {
		case docker.IsNotFound(err):
			continue // 刚好被删掉了
		case err != nil:
			return s.StepFailed("读不到新服务器上的容器 "+name+"，没法确认它有没有在用要写入的数据（还没有改动任何东西）", err.Error(), "确认 Docker 运行正常后重新运行")
		case !usesData(full, volumes, dirs):
			continue
		case kept:
			return s.StepFailed("保留的已有容器 "+name+" 和要迁入的容器共用数据，迁入会覆盖它正在用的数据（还没有改动任何东西）", "",
				"重新运行迁入，检查时把共用数据的容器一起跳过，或者替换这个已有的容器")
		}
		writers = append(writers, writer{summary: c, full: full})
	}
	r.buildGroups(writers)
	r.writers, r.replaceProjects, r.existingProjects = writers, replaceProjects, projects
	return r.checkStoppable(ctx)
}

// checkStoppable 确认迁入时要停下的已有容器都能停下、之后再原样恢复（规则见
// spec.StopRiskOf）：共用数据的容器，以及要被替换的同名容器和同名 Compose 项目的容器。
func (r *run) checkStoppable(ctx context.Context) error {
	type stopping struct {
		c   *docker.Container
		why string
	}
	var list []stopping
	for _, w := range r.writers {
		list = append(list, stopping{w.full, "它和迁入的服务共用数据"})
	}
	var ids []string
	for _, it := range r.items {
		if r.targets[it.m.Name] {
			ids = append(ids, it.m.Name)
		}
	}
	for _, p := range r.replaceProjects {
		for _, c := range r.existingProjects[p] {
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

// dataTargets 是这次要写入的数据卷和目录。
func (r *run) dataTargets() (map[string]bool, []string) {
	vols := map[string]bool{}
	var binds []string
	usedV, usedB := r.usedData()
	for _, v := range r.m.Volumes {
		if !v.Anonymous && usedV[v.Name] && !r.d.KeepVolume(v.Name) {
			vols[v.Name] = true
		}
	}
	for _, b := range r.m.Binds {
		if usedB[b.Path] && !spec.IsSystemPath(b.Path) {
			binds = append(binds, b.Path)
		}
	}
	return vols, binds
}

// writeTarget 是迁入要写到宿主机上的一处，key 是它在分组里的键：挂载目录是 b:路径，
// Compose 项目的目录和项目目录外的文件是 p:项目名。
type writeTarget struct{ key, path string }

// writeSet 是迁入要写入的全部地方：数据卷，以及宿主机上的挂载目录、Compose 项目目录和
// 项目目录外的文件。新服务器上和它们共用数据的已有容器要先停下（保留的服务则不能迁入），
// 分组时也和写入它们的服务放在一组。
func (r *run) writeSet() (map[string]bool, []writeTarget) {
	vols, binds := r.dataTargets()
	var dirs []writeTarget
	for _, b := range binds {
		dirs = append(dirs, writeTarget{"b:" + b, b})
	}
	for _, p := range r.projects {
		for _, path := range append([]string{p.WorkingDir}, p.Outside...) {
			if !spec.IsSystemPath(path) {
				dirs = append(dirs, writeTarget{"p:" + p.Name, path})
			}
		}
	}
	return vols, dirs
}

// usedData 是要迁入的容器用到的卷和目录（被跳过的容器独占的数据不写入）。
func (r *run) usedData() (map[string]bool, map[string]bool) {
	vols, binds := map[string]bool{}, map[string]bool{}
	bindOf := map[string]string{}
	for _, b := range r.m.Binds {
		bindOf[b.Path] = b.Path
		for _, l := range b.AllLinks() {
			bindOf[l.Link] = b.Path
		}
	}
	for _, it := range r.items {
		for _, m := range it.c.Mounts {
			switch m.Type {
			case "volume":
				vols[m.Name] = true
			case "bind":
				for src, real := range bindOf {
					if m.Source == src || strings.HasPrefix(m.Source, src+"/") {
						binds[real] = true
					}
				}
			}
		}
	}
	for _, p := range r.projects {
		for _, b := range r.m.Binds {
			if within(p.WorkingDir, b.Path) {
				binds[b.Path] = true // 项目目录在这个挂载目录里面
			}
		}
	}
	return vols, binds
}

func usesData(c *docker.Container, vols map[string]bool, dirs []writeTarget) bool {
	for _, m := range c.Mounts {
		switch m.Type {
		case "volume":
			if vols[m.Name] {
				return true
			}
		case "bind":
			for _, d := range dirs {
				if spec.PathsOverlap(preflight.Normalize(m.Source), preflight.Normalize(d.path)) {
					return true
				}
			}
		}
	}
	return false
}
