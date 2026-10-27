package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
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
// b: 目录、n: 网络、o: 项目目录以外的文件、w: 临时停止的已有容器。
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
func (r *run) buildGroups(writers []writer) {
	uf := unionFind{}
	binds := []bundle.Bind{}
	for _, b := range r.m.Binds {
		if !spec.IsSystemPath(b.Path) {
			binds = append(binds, b)
		}
	}
	bindKeys := func(source string) []string {
		var keys []string
		for _, b := range binds {
			match := spec.PathsOverlap(source, b.Path)
			for _, l := range b.AllLinks() {
				match = match || spec.PathsOverlap(source, l.Link)
			}
			if match {
				keys = append(keys, "b:"+b.Path)
			}
		}
		return keys
	}
	names := map[string]bool{}
	for _, it := range r.items {
		names[it.m.Name] = true
	}
	for _, it := range r.items {
		c := "c:" + it.m.Name
		uf.find(c)
		if it.m.Project != "" && r.projectRestored(it.m.Project) {
			uf.union(c, "p:"+it.m.Project)
		}
		for _, m := range it.c.Mounts {
			switch m.Type {
			case "volume":
				uf.union(c, "v:"+m.Name)
			case "bind":
				for _, k := range bindKeys(m.Source) {
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
			if n := r.nameOfOld(ref); n != "" {
				dep = n
			}
			if names[dep] {
				uf.union(c, "c:"+dep)
			}
		}
	}
	for _, p := range r.projects {
		k := "p:" + p.Name
		uf.find(k)
		for _, b := range binds {
			if within(p.WorkingDir, b.Path) {
				uf.union(k, "b:"+b.Path)
			}
		}
		for _, o := range p.Outside {
			uf.union(k, "o:"+o)
		}
	}
	vols, bindPaths := r.dataTargets()
	for _, w := range writers {
		k := "w:" + w.summary.Name()
		uf.find(k)
		for _, m := range w.full.Mounts {
			switch {
			case m.Type == "volume" && vols[m.Name]:
				uf.union(k, "v:"+m.Name)
			case m.Type == "bind":
				for _, b := range bindPaths {
					if spec.PathsOverlap(m.Source, b) {
						uf.union(k, "b:"+b)
					}
				}
			}
		}
	}

	byRoot := map[string]*group{}
	r.groups = nil
	get := func(key string) *group {
		root := uf.find(key)
		g := byRoot[root]
		if g == nil {
			g = &group{id: fmt.Sprint(len(r.groups) + 1)}
			byRoot[root] = g
			r.groups = append(r.groups, g)
		}
		return g
	}
	for _, it := range r.items {
		g := get("c:" + it.m.Name)
		g.items = append(g.items, it)
	}
	for _, p := range r.projects {
		g := get("p:" + p.Name)
		g.projects = append(g.projects, p)
	}
	r.groupOf = map[string]string{}
	for k := range uf {
		if g := byRoot[uf.find(k)]; g != nil {
			r.groupOf[k] = g.id
		}
	}
	if len(r.groups) > 1 {
		var parts []string
		for _, g := range r.groups {
			parts = append(parts, g.label())
		}
		r.Log.Printf("分成 %d 组（每组单独启动）：%s", len(r.groups), strings.Join(parts, " | "))
	}
}

// in 表示接下来的改动属于 key（例如 "c:web"）所在的组。
func (r *run) in(key string) {
	if r.j != nil {
		r.j.group = r.groupOf[key]
	}
}

func (r *run) groupByID(id string) *group {
	for _, g := range r.groups {
		if g.id == id {
			return g
		}
	}
	return nil
}

// splitGroups 分出正常运行的和没能启动的组。
func (r *run) splitGroups() (ok, failed []*group) {
	for _, g := range r.groups {
		if g.reason == "" {
			ok = append(ok, g)
		} else {
			failed = append(failed, g)
		}
	}
	return ok, failed
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
	}, 1)
	if err != nil {
		return false, err
	}
	return c == 2, nil
}

// commitPartial 只删除没启动的组、提交其他组。顺序：
//  1. 恢复临时停下的、属于保留下来的组的容器（失败就整体删除，和全部成功时一样）；
//  2. 日志里只留下没启动的组的改动（之后被强制结束，下次运行只会删除它们）；
//  3. 按日志删除没启动的组；
//  4. 删除保留下来的组的回滚资料（被替换的旧容器、回滚镜像、旧目录）。有删不掉的，
//     日志改写成“已提交”加上这些记录，和全部成功时一样，下次运行迁入时接着删；
//  5. 记下已经迁入的服务，重试时跳过。
func (r *run) commitPartial(ctx context.Context, ok, failed []*group) error {
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
			return fail(s, "新服务已验证，但临时停下的容器 "+e.Name+" 没能恢复", err.Error(), "")
		}
	}
	entries, err := readJournal(r.rbDir)
	if err != nil {
		return fail(s, "事务日志读不了", err.Error(), "")
	}
	var keep, drop []Entry
	for _, e := range entries {
		if e.Op == opStart || failedID[e.Group] {
			drop = append(drop, e)
		} else {
			keep = append(keep, e)
		}
	}
	r.j.close()
	if err := rewriteJournal(r.rbDir, drop); err != nil {
		return fail(s, "写入事务日志失败", err.Error(), "")
	}
	// 从这里开始，保留下来的组就算迁入完成了，不再整体删除。
	r.committed, r.partial = true, true
	ctx = context.WithoutCancel(ctx)
	s.Note("删除没启动的服务")
	r.rollbackIssues = r.undo.rollback(ctx, drop)
	r.rollbackDone = true
	r.cleanupLeft = r.undo.cleanupCommitted(ctx, keep)
	switch {
	case len(r.rollbackIssues) > 0:
		// 日志里还是没启动的组的改动，下次运行迁入时接着删除它们。
		// 保留下来的组没删掉的回滚资料只能在结果里列出来，交给人工处理。
	case len(r.cleanupLeft) > 0:
		if err := rewriteJournal(r.rbDir, append(keep, Entry{Op: opCommit, Time: time.Now()})); err != nil {
			r.cleanupLeft = append(r.cleanupLeft, "事务日志没能改写："+err.Error())
		}
	default:
		if err := os.RemoveAll(r.rbDir); err != nil {
			r.Log.Printf("回滚资料目录没能删除：%v", err)
		}
	}
	var items []*citem
	for _, g := range ok {
		items = append(items, g.items...)
	}
	if err := r.saveDone(items); err != nil {
		s.Warn("没能记下已经迁入的服务（%v），重新迁入时会提示它们已存在", err)
	}
	r.warnKept(items)
	if len(r.rollbackIssues) > 0 {
		s.Fail()
	} else {
		s.End("保留 " + labels(ok) + " · 已删除 " + labels(failed))
	}
	return errPartial
}

// errPartial 表示只迁入了一部分（用户选择保留已经正常运行的服务）。
var errPartial = fmt.Errorf("只迁入了一部分")

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
func (r *run) saveDone(items []*citem) error {
	st := r.readDone()
	seen := map[string]bool{}
	var list []doneContainer
	for _, it := range items {
		proj := ""
		if it.m.Project != "" && r.projectRestored(it.m.Project) {
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
func (r *run) skipDone(ctx context.Context) {
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
			if projectOK[c.Project] && !has(r.d.SkipProjects, c.Project) {
				r.d.SkipProjects = append(r.d.SkipProjects, c.Project)
				r.done = append(r.done, "Compose 项目 "+c.Project)
			}
		case alive[c.Name]:
			if !has(r.d.SkipContainers, c.Name) {
				r.d.SkipContainers = append(r.d.SkipContainers, c.Name)
				r.done = append(r.done, c.Name)
			}
		}
	}
	if len(r.done) > 0 {
		r.Screen.Info("上次已经迁入、这次跳过：%s", strings.Join(r.done, "、"))
	}
}
