package restore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/spec"
)

// 启动顺序：一组服务里的每个 Compose 项目、每个独立容器是一个“启动单位”，这里找出单位之间的
// 依赖，排出创建和启动的先后（按这个顺序创建、启动在 startup.go）。依赖分三种，强弱不同，
// 各管各的事：
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

// dependencyOrder 按依赖排序，被依赖的排在前面（例如被借用网络、--link、--volumes-from
// 的容器先创建）。deps 返回一项依赖的其他项的名字。循环依赖时按原来的顺序。
func dependencyOrder[T any](items []T, name func(T) string, deps func(T) []string) []T {
	in := map[string]bool{}
	for _, it := range items {
		in[name(it)] = true
	}
	done := map[string]bool{}
	var out []T
	pending := append([]T(nil), items...)
	for len(pending) > 0 {
		var next []T
		progressed := false
		for _, it := range pending {
			blocked := false
			for _, dep := range deps(it) {
				if dep != name(it) && in[dep] && !done[dep] {
					blocked = true
				}
			}
			if blocked {
				next = append(next, it)
				continue
			}
			out = append(out, it)
			done[name(it)] = true
			progressed = true
		}
		if !progressed {
			return append(out, next...)
		}
		pending = next
	}
	return out
}

// containerDeps 是容器依赖的其他容器（借用网络、--link、--volumes-from），nameOf 把引用的
// ID 换成名字（认不出的原样保留）。
func containerDeps(c *docker.Container, nameOf func(string) string) []string {
	var out []string
	for _, ref := range spec.Dependencies(c) {
		if n := nameOf(ref); n != "" {
			ref = n
		}
		out = append(out, ref)
	}
	return out
}
