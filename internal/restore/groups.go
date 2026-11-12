package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/spec"
)

// 一次迁入多个服务时，按“能不能分开”把它们分成组：同一个 Compose 项目、共用数据卷
// 或目录、共用网络、互相依赖（借用网络、--link、--volumes-from）的容器在同一组。
// 某一组没能启动时，其他组照常启动；最后让用户选择全部删除（默认），还是只删除
// 没启动的组、保留已经正常运行的。事务日志里的每条记录都记着属于哪一组，只删除
// 一部分时按组撤销。

// group 是必须一起迁入的一组服务。
type group struct {
	id       string
	items    []*citem
	projects []bundle.Project
	reason   string // 没能启动的原因；为空表示正常运行
}

// label 是给用户看的名字，例如“Compose 项目 blog”或“web、redis”。
func (g *group) label() string {
	var parts []string
	for _, p := range g.projects {
		parts = append(parts, "Compose 项目 "+p.Name)
	}
	for _, it := range g.items {
		if !g.hasProject(it.m.Project) {
			parts = append(parts, it.m.Name)
		}
	}
	return strings.Join(parts, "、")
}

func (g *group) hasProject(name string) bool {
	for _, p := range g.projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

func labels(list []*group) string {
	var parts []string
	for _, g := range list {
		parts = append(parts, g.label())
	}
	return strings.Join(parts, "、")
}

// unionFind 把有关联的东西合并到同一组。键带前缀：c: 容器、p: 项目、v: 数据卷、
// b: 目录、n: 网络、w: 临时停止的已有容器。
type unionFind map[string]string

func (u unionFind) find(k string) string {
	if _, ok := u[k]; !ok {
		u[k] = k
	}
	for u[k] != k {
		u[k] = u[u[k]]
		k = u[k]
	}
	return k
}

func (u unionFind) union(a, b string) { u[u.find(a)] = u.find(b) }

// writer 是新服务器上正在运行、和要写入的数据共用卷或目录的容器，迁入时要临时停止。
type writer struct {
	summary docker.ContainerSummary
	full    *docker.Container
}

// buildGroups 给要迁入的容器和项目分组，并记下每个卷、目录、网络、临时停止的容器属于哪一组。
func (sc *scope) buildGroups(chk *checked, pkg *bundleFiles, logf func(string, ...any)) {
	uf := unionFind{}
	binds := []bundle.Bind{}
	for _, b := range pkg.m.Binds {
		if !spec.IsSystemPath(b.Path) {
			binds = append(binds, b)
		}
	}
	sc.linkItems(uf, binds, pkg)
	sc.linkPlaces(uf, binds)
	sc.linkWriters(uf, chk)
	sc.groupsFrom(uf, logf)
}

// overlap 表示两个路径是不是同一处或者互相包含。按新服务器上解析软链接之后的实际位置比较：父目录是
// 软链接时，写法不同的两个路径可能是同一处。
func overlap(a, b string) bool { return spec.PathsOverlap(host.RealPath(a), host.RealPath(b)) }

// bindKeys 是挂载 source 会用到的迁入的目录（binds 里的，连同指向它的软链接）。
func bindKeys(binds []bundle.Bind, source string) []string {
	var keys []string
	for _, b := range binds {
		match := overlap(source, b.Path)
		for _, l := range b.AllLinks() {
			match = match || overlap(source, l.Link)
		}
		if match {
			keys = append(keys, "b:"+b.Path)
		}
	}
	return keys
}

// linkItems 把迁入的容器和它所在的 Compose 项目、用到的数据卷、目录、网络、依赖的其他迁入的容器
// 合到一组。
func (sc *scope) linkItems(uf unionFind, binds []bundle.Bind, pkg *bundleFiles) {
	names := map[string]bool{}
	for _, it := range sc.items {
		names[it.m.Name] = true
	}
	for _, it := range sc.items {
		c := "c:" + it.m.Name
		uf.find(c)
		if it.m.Project != "" && sc.projectRestored(it.m.Project) {
			uf.union(c, "p:"+it.m.Project)
		}
		for _, m := range it.c.Mounts {
			switch m.Type {
			case "volume":
				uf.union(c, "v:"+m.Name)
			case "bind":
				for _, k := range bindKeys(binds, m.Source) {
					uf.union(c, k)
				}
			}
		}
		for n := range it.c.NetworkSettings.Networks {
			if n != "bridge" && n != "host" && n != "none" {
				uf.union(c, "n:"+n)
			}
		}
		for _, ref := range spec.Dependencies(it.c) {
			dep := ref
			if n := pkg.nameOfOld(ref); n != "" {
				dep = n
			}
			if names[dep] {
				uf.union(c, "c:"+dep)
			}
		}
	}
	// 替换的 Compose 项目里有和迁入的容器同名的已有容器时（见 preflight.Counterparts），迁入的
	// 容器用的是它空出来的名字：只撤销项目时要把它放回去，名字得先空出来，所以在同一组。
	for _, p := range sc.replaceProjects {
		for _, c := range sc.existingProjects[p] {
			if names[c.Name()] {
				uf.union("c:"+c.Name(), "p:"+p)
			}
		}
	}
}

// linkPlaces 把写到同一处的合到一组：路径相同或者互相包含都算（项目目录、项目目录以外的文件、
// 挂载目录和指向它的软链接）。不然只删除其中一组时，会删掉或换回另一组还在用的文件。
func (sc *scope) linkPlaces(uf unionFind, binds []bundle.Bind) {
	type place struct{ key, path string }
	var places []place
	at := func(key, path string) { places = append(places, place{key, host.RealPath(path)}) }
	for _, b := range binds {
		at("b:"+b.Path, b.Path)
		for _, l := range b.AllLinks() {
			at("b:"+b.Path, l.Link)
		}
	}
	for _, p := range sc.projects {
		k := "p:" + p.Name
		uf.find(k)
		at(k, p.WorkingDir)
		for _, o := range p.Outside {
			at(k, o)
		}
	}
	for i, a := range places {
		for _, b := range places[i+1:] {
			if a.key != b.key && spec.PathsOverlap(a.path, b.path) {
				uf.union(a.key, b.key)
			}
		}
	}
}

// linkWriters 把迁入时要临时停下的已有容器（writers）和它共用的数据卷、目录合到一组。借用别的要停下的
// 容器的网络、进程空间的，和它在同一组：放回它之前，借用的不能先启动。
func (sc *scope) linkWriters(uf unionFind, chk *checked) {
	writers := sc.writers
	if len(writers) == 0 {
		return
	}
	vols, targets := writeSet(sc.writes(chk))
	for _, w := range writers {
		k := "w:" + w.summary.Name()
		uf.find(k)
		for _, ref := range spec.Dependencies(w.full) {
			for _, o := range writers {
				if refersTo(ref, o) {
					uf.union(k, "w:"+o.summary.Name())
				}
			}
		}
		linkMounts(uf, k, w.full.Mounts, vols, targets)
	}
}

// linkMounts 把 k（一个要临时停下的已有容器）和它挂载的、要写入数据的数据卷、目录合到一组。
func linkMounts(uf unionFind, k string, mounts []docker.Mount, vols map[string]bool, targets []writeTarget) {
	for _, m := range mounts {
		switch {
		case m.Type == "volume" && vols[m.Name]:
			uf.union(k, "v:"+m.Name)
		case m.Type == "bind":
			for _, t := range targets {
				if overlap(m.Source, t.path) {
					uf.union(k, t.key)
				}
			}
		}
	}
}

// groupsFrom 按 uf 合出来的组建出 sc.groups（按迁入的容器、项目的先后编号）和 sc.groupOf（每个键在哪一组）。
func (sc *scope) groupsFrom(uf unionFind, logf func(string, ...any)) {
	byRoot := map[string]*group{}
	sc.groups = nil
	get := func(key string) *group {
		root := uf.find(key)
		g := byRoot[root]
		if g == nil {
			g = &group{id: fmt.Sprint(len(sc.groups) + 1)}
			byRoot[root] = g
			sc.groups = append(sc.groups, g)
		}
		return g
	}
	for _, it := range sc.items {
		g := get("c:" + it.m.Name)
		g.items = append(g.items, it)
	}
	for _, p := range sc.projects {
		g := get("p:" + p.Name)
		g.projects = append(g.projects, p)
	}
	sc.groupOf = map[string]string{}
	for k := range uf {
		if g := byRoot[uf.find(k)]; g != nil {
			sc.groupOf[k] = g.id
		}
	}
	if len(sc.groups) > 1 {
		var parts []string
		for _, g := range sc.groups {
			parts = append(parts, g.label())
		}
		logf("分成 %d 组（每组单独启动）：%s", len(sc.groups), strings.Join(parts, " | "))
	}
}

// in 表示接下来的改动属于 key（例如 "c:web"）所在的组。
func (r *migrate) in(key string) {
	if r.j != nil {
		r.j.group = r.sc.groupOf[key]
	}
}

func (sc *scope) groupByID(id string) *group {
	for _, g := range sc.groups {
		if g.id == id {
			return g
		}
	}
	return nil
}

// splitGroups 分出正常运行的和没能启动的组。
func (sc *scope) splitGroups() (ok, failed []*group) {
	for _, g := range sc.groups {
		if g.reason == "" {
			ok = append(ok, g)
		} else {
			failed = append(failed, g)
		}
	}
	return ok, failed
}

// ---------- 重试时跳过已经迁入的服务 ----------

// doneState 记着同一个迁移链接上次已经迁入的服务，重试时跳过它们。
type doneState struct {
	Containers []doneContainer `json:"containers"`
}

type doneContainer struct {
	Name    string `json:"name"`
	ID      string `json:"id"`                // 新服务器上的容器 ID
	Project string `json:"project,omitempty"` // 作为 Compose 项目迁入时的项目名
}

func (r *run) donePath() string {
	return filepath.Join(r.Home, "state", "in-"+r.opts.Link.Token+".json")
}

func (r *run) readDone() doneState {
	var st doneState
	if b, err := os.ReadFile(r.donePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// saveDone 把这次保留下来的服务加进记录。
func (r *migrate) saveDone(items []*citem) error {
	st := r.readDone()
	seen := map[string]bool{}
	var list []doneContainer
	for _, it := range items {
		proj := ""
		if it.m.Project != "" && r.sc.projectRestored(it.m.Project) {
			proj = it.m.Project
		}
		list = append(list, doneContainer{Name: it.m.Name, ID: it.newID, Project: proj})
		seen[it.m.Name] = true
	}
	for _, c := range st.Containers {
		if !seen[c.Name] {
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(doneState{Containers: list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.donePath()), 0o700); err != nil {
		return err
	}
	tmp := r.donePath() + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.donePath())
}

func (r *run) clearDone() { _ = os.Remove(r.donePath()) }

// skipDone 读出上次已经迁入的服务，这次跳过它们。容器被删掉或换成了别的容器时，
// 照常重新迁入；Compose 项目的容器少了任何一个，整个项目重新迁入。
func (r *run) skipDone(ctx context.Context, chk *checked) {
	st := r.readDone()
	if len(st.Containers) == 0 {
		return
	}
	alive := map[string]bool{}
	for _, c := range st.Containers {
		// 按 ID 找：Compose 副本在新服务器上的编号（名字）可能和旧服务器上不一样。
		_, _, err := r.Docker.InspectContainer(ctx, c.ID)
		alive[c.Name] = err == nil
	}
	projectOK := map[string]bool{}
	for _, c := range st.Containers {
		if c.Project == "" {
			continue
		}
		if _, seen := projectOK[c.Project]; !seen {
			projectOK[c.Project] = true
		}
		projectOK[c.Project] = projectOK[c.Project] && alive[c.Name]
	}
	for _, c := range st.Containers {
		switch {
		case c.Project != "":
			if projectOK[c.Project] && !slices.Contains(chk.d.SkipProjects, c.Project) {
				chk.d.SkipProjects = append(chk.d.SkipProjects, c.Project)
				chk.done = append(chk.done, "Compose 项目 "+c.Project)
			}
		case alive[c.Name]:
			if !slices.Contains(chk.d.SkipContainers, c.Name) {
				chk.d.SkipContainers = append(chk.d.SkipContainers, c.Name)
				chk.done = append(chk.done, c.Name)
			}
		}
	}
	if len(chk.done) > 0 {
		r.Screen.Info("上次已经迁入、这次跳过：%s", strings.Join(chk.done, "、"))
	}
}

func (sc *scope) projectRestored(name string) bool {
	for _, p := range sc.projects {
		if p.Name == name {
			return true
		}
	}
	return false
}
