package restore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 迁入要写到的地方包括 Compose 项目的目录和项目目录外的文件：新服务器上用着它们的已有容器
// 要先停下，保留的服务则不能迁入（不然项目文件会盖掉它正在用的配置）。
func TestWriteSetIncludesProjectFiles(t *testing.T) {
	r := &run{m: &bundle.Manifest{}, projects: []bundle.Project{{Name: "blog", WorkingDir: "/srv/blog", Outside: []string{"/etc/blog.env"}}}}
	vols, dirs := r.writeSet()
	if len(vols) != 0 || fmt.Sprint(dirs) != "[{p:blog /srv/blog} {p:blog /etc/blog.env}]" {
		t.Fatalf("要写的地方：%v %v", vols, dirs)
	}
	c := &docker.Container{Mounts: []docker.Mount{{Type: "bind", Source: "/srv/blog/config"}}}
	if !usesData(c, vols, dirs) {
		t.Error("挂载了项目目录里的子目录，也算在用要写入的数据")
	}
}

// 和迁入的数据共用的已有容器要先停下：用 --rm 启动的一停就被 Docker 删掉，Swarm 的任务停了会
// 被换成新的，都恢复不了。要在改动任何东西之前停下，也不能去停它。
func TestDecideRefusesUnstoppableWriters(t *testing.T) {
	for _, c := range []struct{ name, extra, want string }{
		{"--rm", `"HostConfig":{"AutoRemove":true},"Config":{"Labels":{}}`, "是用 --rm 启动的"},
		{"Swarm", `"HostConfig":{},"Config":{"Labels":{"com.docker.swarm.service.id":"x","com.docker.swarm.service.name":"web"}}`,
			"属于 Swarm 服务 web"},
	} {
		failed, stopped := prepareWith(t, preflight.Decisions{}, func(w http.ResponseWriter, p, shared string) bool {
			switch p {
			case "/containers/json":
				io.WriteString(w, `[{"Id":"other-id","Names":["/other"],"State":"running"}]`)
			case "/containers/other-id/json":
				io.WriteString(w, `{"Id":"other-id","Name":"/other","State":{"Status":"running","Running":true},`+c.extra+
					`,"Mounts":[{"Type":"bind","Source":"`+shared+`"}]}`)
			default:
				return false
			}
			return true
		})
		if !strings.Contains(failed, c.want) || !strings.Contains(failed, "还没有改动任何东西") || len(stopped) > 0 {
			t.Errorf("%s：应在改动之前停下：%q，停过 %v", c.name, failed, stopped)
		}
	}
}

// decide 之后、改动之前（加载镜像可能要好一会儿），新服务器上启动了一个在用要写入的目录的
// 容器：照 decide 时的样子做，就不会停下它，数据写进去时它还在用。要在动已有的服务和数据之前
// 发现，停下。
func TestRecheckBeforePrepare(t *testing.T) {
	lists := 0
	failed, stopped := prepareWith(t, preflight.Decisions{}, func(w http.ResponseWriter, p, shared string) bool {
		switch p {
		case "/containers/json":
			if lists++; lists == 1 {
				io.WriteString(w, `[]`)
			} else {
				io.WriteString(w, `[{"Id":"late-id","Names":["/late"],"State":"running"}]`)
			}
		case "/containers/late-id/json":
			io.WriteString(w, `{"Id":"late-id","Name":"/late","State":{"Status":"running","Running":true},"Mounts":[{"Type":"bind","Source":"`+shared+`"}]}`)
		default:
			return false
		}
		return true
	})
	if !strings.Contains(failed, "late 在检查之后有了变化") || len(stopped) > 0 {
		t.Fatalf("应在动已有的服务之前停下（停过 %v）：\n%s", stopped, failed)
	}
}

// 单独迁入的容器 web 和新服务器上 Compose 项目 site 里的容器同名，迁移包里也有 site：已有的 web
// 跟着 site 走，和迁入前检查是同一条规则（见 preflight.Counterparts）。替换 site 时它随项目备份、
// 删除，不再另外改名备份（以前两样都做：项目备份记下的是改过的名字，回滚时找不到改名的备份，又按
// 改过的名字重建它），迁入的 web 和 site 在同一组；保留 site 时它的名字被占着，迁移包里的 web
// 不迁入（以前照样把它停下、改名备份，和检查时说的“保留”相反）。
func TestReplaceFollowsProject(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("保留site=%v", keep), func(t *testing.T) {
			web := `{"Id":"target-web","Name":"/web","State":{"Status":"running","Running":true},"HostConfig":{},` +
				`"Config":{"Image":"nginx","Labels":{"com.docker.compose.project":"site"}}}`
			dc, calls := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
				switch p := strings.TrimPrefix(req.URL.Path, "/v1.44"); {
				case p == "/containers/json":
					io.WriteString(w, `[{"Id":"target-web","Names":["/web"],"State":"running","Labels":{"com.docker.compose.project":"site"}}]`)
				case p == "/containers/target-web/json", p == "/containers/web/json":
					io.WriteString(w, web)
				case p == "/containers/target-web/changes":
					io.WriteString(w, `[]`)
				case req.Method != http.MethodGet && strings.HasPrefix(p, "/containers/target-web"):
					w.WriteHeader(http.StatusNoContent)
				default:
					return false
				}
				return true
			})
			home := t.TempDir()
			data := filepath.Join(home, "unpacked")
			os.MkdirAll(filepath.Join(data, "containers"), 0o700)
			var plan []bundle.PlanContainer
			var containers []bundle.Container
			for _, c := range [][2]string{{"web", ""}, {"site-app-1", "site"}, {"other", ""}} {
				os.WriteFile(filepath.Join(data, "containers", c[0]+".json"), []byte(`{"Id":"source-`+c[0]+`","Name":"/`+c[0]+`","Config":{"Image":"nginx"}}`), 0o600)
				plan = append(plan, bundle.PlanContainer{Name: c[0], Project: c[1]})
				containers = append(containers, bundle.Container{Name: c[0], Project: c[1]})
			}
			var d preflight.Decisions
			if keep {
				d.SkipProjects = []string{"site"}
			}
			var out bytes.Buffer
			screen := ui.New(&out, false, nil)
			r := &run{Deps: Deps{Docker: dc, Home: home, Screen: screen, Getenv: func(string) string { return "" }}, d: d,
				plan:    &bundle.Plan{Containers: plan},
				m:       &bundle.Manifest{Containers: containers, Projects: []bundle.Project{{Name: "site", WorkingDir: filepath.Join(home, "site")}}},
				dataDir: data, rbDir: filepath.Join(home, "rollback", "current"), anonVolumes: map[string]bundle.Volume{},
				undo: &undo{mountPoint: noMounts, dc: dc, screen: screen, logf: t.Logf, fail: func(string) bool { return false }}}
			ctx := context.Background()
			err := r.decide(ctx)
			if err == nil {
				err = r.begin()
			}
			if err == nil {
				err = r.prepare(ctx)
			}
			if r.j != nil {
				r.j.close()
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			var touched, migrating []string
			for _, c := range calls() {
				if !strings.HasPrefix(c, "GET") && strings.Contains(c, "target-web") {
					touched = append(touched, strings.Replace(c, "/v1.44", "", 1))
				}
			}
			for _, it := range r.items {
				migrating = append(migrating, it.m.Name)
			}
			entries, _ := readJournal(r.rbDir)
			var ops []string
			for _, e := range entries {
				ops = append(ops, string(e.Op))
				for _, o := range e.Old {
					ops = append(ops, o.Name)
				}
			}
			want := []string{"POST /containers/target-web/stop", "DELETE /containers/target-web"}
			wantItems, wantOps := "web,site-app-1,other", "start,compose_old,web,old_images"
			if keep {
				want, wantItems, wantOps = nil, "other", "start"
				if len(r.d.Skipped) != 1 || r.d.Skipped[0].Container != "web" || !strings.Contains(r.d.Skipped[0].Reason, "site") {
					t.Errorf("迁移包里的 web 不迁入，要说明原因：%+v", r.d.Skipped)
				}
			}
			if !slices.Equal(touched, want) || strings.Join(migrating, ",") != wantItems || strings.Join(ops, ",") != wantOps {
				t.Errorf("已有的 web：%v，迁入：%v，日志：%v", touched, migrating, ops)
			}
			// 迁入的 web 用的是 site 里已有的 web 空出来的名字：只撤销 site 时要放回已有的 web，
			// 名字得先空出来，所以两者在同一组。
			if !keep && r.groupOf["c:web"] != r.groupOf["p:site"] {
				t.Errorf("迁入的 web 应和替换的 site 在同一组：%v", r.groupOf)
			}
		})
	}
}

// Compose 项目 site 里写了 container_name: nginx 的容器，和新服务器上无关的已有容器 nginx 同名（独立容器，
// 或者属于迁移包里没有的项目）：已有的 nginx 跟着 site 走，和迁入前检查是同一条规则（见
// preflight.Counterparts）。替换 site 时先停下、改名备份它，名字空出来（以前不管它，Compose 创建容器时
// 名字冲突，整体回滚）；--existing skip 时 site 不迁入，--existing fail 时停下，都不动它。
func TestReplaceNamedFollowsProject(t *testing.T) {
	bundled := [][2]string{{"nginx", "site"}, {"site-app-1", "site"}, {"other", ""}}
	for _, owner := range []string{"", "proxy"} {
		for _, c := range []struct {
			name, policy      string
			d                 preflight.Decisions
			touched           []string
			items, ops, fails string
		}{
			{name: "替换", touched: []string{"POST /containers/target-nginx/stop", "POST /containers/target-nginx/rename"},
				items: "nginx,site-app-1,other", ops: "start,stop,rename"},
			{name: "保留", d: preflight.Decisions{SkipProjects: []string{"site"}}, items: "other", ops: "start"},
			{name: "--existing skip", policy: "skip", items: "other", ops: "start"},
			{name: "--existing fail", policy: "fail", fails: "--existing fail 不允许替换"},
		} {
			t.Run(fmt.Sprintf("所属项目%q/%s", owner, c.name), func(t *testing.T) {
				r, failed, touched, ops := decideAndPrepare(t, bundled, [][2]string{{"nginx", owner}}, c.d, c.policy)
				if c.fails != "" {
					if !strings.Contains(failed, c.fails) || len(touched) > 0 {
						t.Errorf("应在改动之前停下（动过 %v）：%q", touched, failed)
					}
					return
				}
				var items []string
				for _, it := range r.items {
					items = append(items, it.m.Name)
				}
				if failed != "" || !slices.Equal(touched, c.touched) || strings.Join(items, ",") != c.items || strings.Join(ops, ",") != c.ops {
					t.Errorf("已有的 nginx：%v，迁入：%v，日志：%v\n%s", touched, items, ops, failed)
				}
				if c.policy == "skip" && (len(r.d.Skipped) != 1 || r.d.Skipped[0].Project != "site" || !strings.Contains(r.d.Skipped[0].Reason, "nginx")) {
					t.Errorf("site 不迁入，要说明原因：%+v", r.d.Skipped)
				}
			})
		}
	}
}

// 已有的 nginx 属于新服务器上的 Compose 项目 proxy，迁移包里也有 proxy：它跟着 proxy 走。保留 proxy 时名字
// 被占着，site 也不迁入，要说明原因，也不用检查它的配置文件（以前照样迁入，Compose 创建容器时名字冲突，
// 整体回滚）；替换 proxy 时它随 proxy 备份、删除，名字空出来给 site，两个项目在同一组。
func TestNameHeldByKeptProject(t *testing.T) {
	bundled := [][2]string{{"nginx", "site"}, {"proxy-app-1", "proxy"}, {"other", ""}}
	existing := [][2]string{{"nginx", "proxy"}}
	r, failed, touched, _ := decideAndPrepare(t, bundled, existing, preflight.Decisions{SkipProjects: []string{"proxy"}}, "")
	if failed != "" || len(touched) > 0 || len(r.items) != 1 || r.items[0].m.Name != "other" || len(r.d.Skipped) != 1 ||
		r.d.Skipped[0].Project != "site" || !strings.Contains(r.d.Skipped[0].Reason, "nginx 属于保留的 Compose 项目 proxy") {
		t.Errorf("保留 proxy 时 site 不迁入：动过 %v，跳过 %+v\n%s", touched, r.d.Skipped, failed)
	}
	if err := r.checkCompose(context.Background()); err != nil {
		t.Errorf("不迁入的项目不用检查配置文件（新服务器没装 Compose 也没关系）：%v", err)
	}
	r, failed, touched, _ = decideAndPrepare(t, bundled, existing, preflight.Decisions{}, "")
	want := []string{"POST /containers/target-nginx/stop", "DELETE /containers/target-nginx"}
	if failed != "" || !slices.Equal(touched, want) || len(r.items) != 3 || r.groupOf["p:site"] != r.groupOf["p:proxy"] {
		t.Errorf("替换 proxy 时 nginx 随它备份、删除，site 和它在同一组：%v %v\n%s", touched, r.groupOf, failed)
	}
}

// decideAndPrepare 按迁入的顺序调用 decide、begin、prepare。bundled 是迁移包里的容器（名字和所属的 Compose
// 项目，单独迁入的为空），existing 是新服务器上已有的容器（同样；都在运行，ID 是 target-名字），policy 是
// --existing。返回 run、出错时屏幕上的输出、对已有容器的改动（GET 以外的请求）和事务日志里的操作。
func decideAndPrepare(t *testing.T, bundled, existing [][2]string, d preflight.Decisions, policy string) (r *run, failed string, touched, ops []string) {
	t.Helper()
	var list []string
	inspect := map[string]string{}
	for _, e := range existing {
		labels := `{}`
		if e[1] != "" {
			labels = `{"com.docker.compose.project":"` + e[1] + `"}`
		}
		list = append(list, `{"Id":"target-`+e[0]+`","Names":["/`+e[0]+`"],"State":"running","Labels":`+labels+`}`)
		js := `{"Id":"target-` + e[0] + `","Name":"/` + e[0] + `","State":{"Status":"running","Running":true},"HostConfig":{},` +
			`"Config":{"Image":"nginx","Labels":` + labels + `}}`
		inspect["/containers/target-"+e[0]+"/json"], inspect["/containers/"+e[0]+"/json"] = js, js
	}
	dc, calls := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		switch p := strings.TrimPrefix(req.URL.Path, "/v1.44"); {
		case p == "/containers/json":
			io.WriteString(w, "["+strings.Join(list, ",")+"]")
		case req.Method == http.MethodGet && inspect[p] != "":
			io.WriteString(w, inspect[p])
		case req.Method == http.MethodGet && strings.HasPrefix(p, "/containers/target-") && strings.HasSuffix(p, "/changes"):
			io.WriteString(w, `[]`)
		case req.Method != http.MethodGet && strings.HasPrefix(p, "/containers/target-"):
			w.WriteHeader(http.StatusNoContent)
		default:
			return false
		}
		return true
	})
	home := t.TempDir()
	data := filepath.Join(home, "unpacked")
	os.MkdirAll(filepath.Join(data, "containers"), 0o700)
	m := &bundle.Manifest{}
	var plan []bundle.PlanContainer
	for _, c := range bundled {
		os.WriteFile(filepath.Join(data, "containers", c[0]+".json"), []byte(`{"Id":"source-`+c[0]+`","Name":"/`+c[0]+`","Config":{"Image":"nginx"}}`), 0o600)
		plan = append(plan, bundle.PlanContainer{Name: c[0], Project: c[1]})
		m.Containers = append(m.Containers, bundle.Container{Name: c[0], Project: c[1]})
		if c[1] != "" && !slices.ContainsFunc(m.Projects, func(p bundle.Project) bool { return p.Name == c[1] }) {
			m.Projects = append(m.Projects, bundle.Project{Name: c[1], WorkingDir: filepath.Join(home, c[1])})
		}
	}
	var out bytes.Buffer
	screen := ui.New(&out, false, nil)
	r = &run{Deps: Deps{Docker: dc, Home: home, Screen: screen, Getenv: func(string) string { return "" }},
		opts: Options{Existing: policy}, d: d, plan: &bundle.Plan{Containers: plan}, m: m,
		dataDir: data, rbDir: filepath.Join(home, "rollback", "current"), anonVolumes: map[string]bundle.Volume{},
		undo: &undo{mountPoint: noMounts, dc: dc, screen: screen, logf: t.Logf, fail: func(string) bool { return false }}}
	ctx := context.Background()
	err := r.decide(ctx)
	if err == nil {
		err = r.begin()
	}
	if err == nil {
		err = r.prepare(ctx)
	}
	if r.j != nil {
		r.j.close()
	}
	if err != nil {
		failed = out.String()
	}
	for _, c := range calls() {
		if !strings.HasPrefix(c, "GET") {
			touched = append(touched, strings.Replace(c, "/v1.44", "", 1))
		}
	}
	entries, _ := readJournal(r.rbDir)
	for _, e := range entries {
		ops = append(ops, string(e.Op))
	}
	return r, failed, touched, ops
}
