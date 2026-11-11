package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/docker/dockertest"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 有关联的服务分在同一组，互不相干的分开；临时停止的已有容器跟着它用到的数据走。
func TestBuildGroups(t *testing.T) {
	mk := func(name, project, js string) *citem {
		var c docker.Container
		if err := json.Unmarshal([]byte(js), &c); err != nil {
			t.Fatal(err)
		}
		c.Name = "/" + name
		return &citem{m: bundle.Container{Name: name, Project: project}, c: &c}
	}
	r := &run{m: &bundle.Manifest{
		Binds:    []bundle.Bind{{Path: "/srv/shared"}, {Path: "/srv/blog"}, {Path: "/var/run/docker.sock"}},
		Volumes:  []bundle.Volume{{Name: "dbdata"}, {Name: "cache"}},
		Projects: []bundle.Project{{Name: "blog", WorkingDir: "/srv/blog/app"}},
	}}
	r.projects = r.m.Projects
	// 迁移计划里记的和迁移包里的一致（见 preflight.WritesOf）。
	r.plan = &bundle.Plan{
		Containers: []bundle.PlanContainer{
			{Name: "web", Binds: []string{"/srv/shared"}}, {Name: "nginx", Binds: []string{"/srv/shared", "/var/run/docker.sock"}},
			{Name: "blog-app-1", Project: "blog"}, {Name: "blog-db-1", Project: "blog", Volumes: []string{"dbdata"}},
			{Name: "files", Binds: []string{"/srv/blog"}}, {Name: "redis", Volumes: []string{"cache"}}, {Name: "exporter"},
			{Name: "alone", Binds: []string{"/var/run/docker.sock"}},
		},
		Projects: []bundle.PlanProject{{Name: "blog", WorkingDir: "/srv/blog/app"}},
		Volumes:  []bundle.PlanVolume{{Name: "dbdata"}, {Name: "cache"}},
		Binds:    []bundle.PlanBind{{Path: "/srv/shared"}, {Path: "/srv/blog"}, {Path: "/var/run/docker.sock"}},
	}
	r.items = []*citem{
		mk("web", "", `{"Mounts":[{"Type":"bind","Source":"/srv/shared/html"}],"NetworkSettings":{"Networks":{"bridge":{}}}}`),
		mk("nginx", "", `{"Mounts":[{"Type":"bind","Source":"/srv/shared"},{"Type":"bind","Source":"/var/run/docker.sock"}]}`),
		mk("blog-app-1", "blog", `{"NetworkSettings":{"Networks":{"blog_default":{}}}}`),
		mk("blog-db-1", "blog", `{"Mounts":[{"Type":"volume","Name":"dbdata"}]}`),
		mk("files", "", `{"Mounts":[{"Type":"bind","Source":"/srv/blog/uploads"}]}`),
		mk("redis", "", `{"Mounts":[{"Type":"volume","Name":"cache"}],"NetworkSettings":{"Networks":{"bridge":{}}}}`),
		mk("exporter", "", `{"HostConfig":{"NetworkMode":"container:redis"}}`),
		mk("alone", "", `{"Mounts":[{"Type":"bind","Source":"/var/run/docker.sock"}]}`),
	}
	var w docker.Container
	json.Unmarshal([]byte(`{"Mounts":[{"Type":"volume","Name":"cache"}]}`), &w)
	r.buildGroups([]writer{{summary: docker.ContainerSummary{Names: []string{"/backup"}}, full: &w}})

	var got []string
	for _, g := range r.groups {
		got = append(got, g.label())
	}
	want := []string{"web、nginx", "Compose 项目 blog、files", "redis、exporter", "alone"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("分组不对：\n现在：%v\n应该：%v", got, want)
	}
	if r.groupOf["v:dbdata"] != r.groupOf["p:blog"] || r.groupOf["b:/srv/blog"] != r.groupOf["p:blog"] {
		t.Error("项目用到的卷、项目目录所在的挂载目录应和项目同组")
	}
	if r.groupOf["w:backup"] != r.groupOf["c:redis"] {
		t.Error("临时停止的容器应和用同一个卷的服务同组")
	}
	if _, ok := r.groupOf["b:/var/run/docker.sock"]; ok {
		t.Error("宿主机系统路径不应把服务连到一起")
	}
}

// 写到同一处的项目必须分在同一组：只删除没启动的那组时，不能删掉或换回保留下来的项目还在用的文件。
func TestBuildGroupsSharedPaths(t *testing.T) {
	cases := []struct {
		name     string
		projects []bundle.Project
		together bool
	}{
		{"同一个目录", []bundle.Project{{Name: "a", WorkingDir: "/srv/app"}, {Name: "b", WorkingDir: "/srv/app"}}, true},
		{"一个在另一个里面", []bundle.Project{{Name: "a", WorkingDir: "/srv/app"}, {Name: "b", WorkingDir: "/srv/app/admin"}}, true},
		{"目录外的文件在另一个项目里", []bundle.Project{{Name: "a", WorkingDir: "/srv/a", Outside: []string{"/srv/b/.env"}}, {Name: "b", WorkingDir: "/srv/b"}}, true},
		{"共用目录外的同一个文件", []bundle.Project{{Name: "a", WorkingDir: "/srv/a", Outside: []string{"/etc/app.env"}}, {Name: "b", WorkingDir: "/srv/b", Outside: []string{"/etc/app.env"}}}, true},
		{"互不相干", []bundle.Project{{Name: "a", WorkingDir: "/srv/a"}, {Name: "b", WorkingDir: "/srv/ab"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &run{m: &bundle.Manifest{Projects: c.projects}, projects: c.projects}
			for _, p := range c.projects {
				r.items = append(r.items, &citem{m: bundle.Container{Name: p.Name + "-web-1", Project: p.Name}, c: &docker.Container{}})
			}
			r.buildGroups(nil)
			if together := r.groupOf["p:a"] == r.groupOf["p:b"]; together != c.together {
				t.Errorf("同组：%v，应该：%v（%d 组）", together, c.together, len(r.groups))
			}
		})
	}
}

// 父目录是软链接时，写法不同的两个路径可能是同一处（还不存在的末级目录、文件也一样），
// 要按实际位置分到同一组。
func TestBuildGroupsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "real"), 0o755)
	os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias"))
	for name, projects := range map[string][]bundle.Project{
		"项目目录": {{Name: "a", WorkingDir: filepath.Join(root, "alias", "app")}, {Name: "b", WorkingDir: filepath.Join(root, "real", "app")}},
		"目录外的文件": {{Name: "a", WorkingDir: filepath.Join(root, "a"), Outside: []string{filepath.Join(root, "alias", "shared.env")}},
			{Name: "b", WorkingDir: filepath.Join(root, "b"), Outside: []string{filepath.Join(root, "real", "shared.env")}}},
	} {
		r := &run{m: &bundle.Manifest{Projects: projects}, projects: projects}
		for _, p := range projects {
			r.items = append(r.items, &citem{m: bundle.Container{Name: p.Name + "-web-1", Project: p.Name}, c: &docker.Container{}})
		}
		r.buildGroups(nil)
		if r.groupOf["p:a"] != r.groupOf["p:b"] {
			t.Errorf("%s：经过软链接写到同一处，应在同一组", name)
		}
	}
}

// 分在不同组的项目互不影响：只删除一组时，另一组的文件原样保留。
func TestPartialRollbackKeepsOtherGroup(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	os.Mkdir(stage, 0o700)
	os.WriteFile(filepath.Join(stage, "compose.yml"), []byte("services: {}"), 0o600)
	projects := []bundle.Project{{Name: "a", WorkingDir: filepath.Join(root, "a")}, {Name: "b", WorkingDir: filepath.Join(root, "b")}}
	var out bytes.Buffer
	r := &run{Deps: Deps{Screen: ui.New(&out, false, nil)}, m: &bundle.Manifest{Projects: projects}, projects: projects}
	for _, p := range projects {
		r.items = append(r.items, &citem{m: bundle.Container{Name: p.Name + "-web-1", Project: p.Name}, c: &docker.Container{}})
	}
	r.buildGroups(nil)
	j, err := openJournal(filepath.Join(root, "rb"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	r.j = j
	for _, p := range projects {
		r.in("p:" + p.Name)
		if err := r.installTree(stage, p.WorkingDir, nil); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := readJournal(j.dir)
	if err != nil {
		t.Fatal(err)
	}
	var drop []Entry
	for _, e := range entries {
		if e.Group == r.groupOf["p:a"] {
			drop = append(drop, e)
		}
	}
	u := &undo{mountPoint: noMounts, screen: r.Screen, logf: t.Logf, fail: func(string) bool { return false }}
	if problems := u.rollback(context.Background(), drop); len(problems) > 0 {
		t.Fatal(problems)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "compose.yml")); !os.IsNotExist(err) {
		t.Errorf("删除的那组应删掉：%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "b", "compose.yml")); err != nil || string(b) != "services: {}" {
		t.Errorf("保留的那组应原样保留：%q %v", b, err)
	}
}

// 只保留一部分服务时，只有没保留的那组用到的镜像，名字也要改回去、迁入带来的镜像删掉；
// 几组都用的镜像名留着。
func TestPartialRollbackRestoresImageNames(t *testing.T) {
	var out bytes.Buffer
	r := &run{Deps: Deps{Screen: ui.New(&out, false, nil)}, m: &bundle.Manifest{}}
	for _, name := range []string{"good", "bad"} {
		r.items = append(r.items, &citem{m: bundle.Container{Name: name, ImageID: "sha256:src-" + name}, c: &docker.Container{}})
	}
	r.items[0].c.Config.Image, r.items[1].c.Config.Image = "base:1", "base:1" // 两组都用 base:1
	r.buildGroups(nil)
	if r.imageKey("sha256:src-bad", "bad:1") != "c:bad" || r.imageKey("sha256:src-base", "base:1") != "" {
		t.Fatal("只有一组用的镜像跟着那一组，两组都用的算共用")
	}
	d := &images{tags: map[string]string{"good:1": "sha256:new-good", "bad:1": "sha256:new-bad", "base:1": "sha256:new-base"},
		ids: map[string]bool{"sha256:old-bad": true, "sha256:new-good": true, "sha256:new-bad": true, "sha256:new-base": true}}
	r.undo = &undo{mountPoint: noMounts, dc: d.docker(t), screen: r.Screen, logf: t.Logf, fail: func(string) bool { return false }}
	j, err := openJournal(filepath.Join(t.TempDir(), "rb"))
	if err != nil {
		t.Fatal(err)
	}
	r.j, r.rbDir = j, j.dir
	for _, e := range []Entry{{Op: opStart}, {Op: opImages, Names: []string{"sha256:old-bad"}}} {
		j.add(e)
	}
	for _, x := range []struct{ id, ref, was string }{{"sha256:src-good", "good:1", ""}, {"sha256:src-bad", "bad:1", "sha256:old-bad"}, {"sha256:src-base", "base:1", ""}} {
		r.in(r.imageKey(x.id, x.ref))
		j.add(Entry{Op: opTag, Name: x.ref, ID: x.was})
	}
	ok, bad := r.groupByID(r.groupOf["c:good"]), r.groupByID(r.groupOf["c:bad"])
	bad.reason = "没能启动"
	r.Home = t.TempDir()
	if err := r.commitPartial(context.Background(), []*group{ok}, []*group{bad}); err != errPartial {
		t.Fatal(err)
	}
	want := map[string]string{"good:1": "sha256:new-good", "bad:1": "sha256:old-bad", "base:1": "sha256:new-base"}
	if !maps.Equal(d.tags, want) || strings.Join(d.deleted, ",") != "sha256:new-bad" {
		t.Fatalf("镜像名 %v，删掉的 %v", d.tags, d.deleted)
	}
}

// 只保留一部分服务时，镜像名按创建容器实际用的名字算是哪组的：app 和 app:latest 是同一个名字，
// 两组都在用就留着；容器用的是旧版本的镜像（OldImage）时，创建它用的是那个旧版本的名字，配置里
// 原来写的名字只算真正用它的那组的，那组没保留就改回去。
func TestPartialRollbackImageNameUsers(t *testing.T) {
	cases := []struct {
		name      string
		good, bad bundle.Container // Image 是容器配置里写的镜像
		files     []bundle.ImageFile
		want      map[string]string // 只保留 good 之后的镜像名
	}{
		{"同一个名字的两种写法",
			bundle.Container{Name: "good", Image: "app", ImageID: "sha256:src"},
			bundle.Container{Name: "bad", Image: "app:latest", ImageID: "sha256:src"},
			[]bundle.ImageFile{{File: "src", ID: "sha256:src", Refs: []string{"app", "app:latest"}}},
			map[string]string{"app:latest": "sha256:src"}},
		{"保留的那组用旧版本的镜像",
			bundle.Container{Name: "good", Image: "app:1", ImageID: "sha256:src-old", OldImage: "dockhop-image:123456789abc"},
			bundle.Container{Name: "bad", Image: "app:1", ImageID: "sha256:src"},
			[]bundle.ImageFile{{File: "old", ID: "sha256:src-old", Refs: []string{"dockhop-image:123456789abc"}},
				{File: "src", ID: "sha256:src", Refs: []string{"app:1"}}},
			map[string]string{"app:1": "sha256:target", "dockhop-image:123456789abc": "sha256:src-old"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 新服务器上原来就有这个名字，指向另一个镜像。
			d := &images{tags: map[string]string{dockerName(c.bad.Image): "sha256:target"},
				ids: map[string]bool{"sha256:target": true, "sha256:src": true, "sha256:src-old": true}}
			var out bytes.Buffer
			r := &run{Deps: Deps{Docker: d.docker(t), Screen: ui.New(&out, false, nil), Home: t.TempDir()}, imageAs: map[string]string{},
				m: &bundle.Manifest{ImageFiles: c.files, Images: bundle.Images{Refs: map[string]string{}}}, images: map[string]imageSource{}}
			for _, x := range []bundle.Container{c.good, c.bad} {
				it := &citem{m: x, c: &docker.Container{}}
				it.c.Config.Image = x.Image
				r.items = append(r.items, it)
			}
			r.buildGroups(nil)
			for _, f := range c.files {
				for _, ref := range f.Refs {
					r.m.Images.Refs[ref] = f.ID
				}
				r.images[f.File] = imageSource{local: f.ID} // 本机已经有，只加名字
			}
			r.undo = &undo{mountPoint: noMounts, dc: r.Docker, screen: r.Screen, logf: t.Logf, fail: func(string) bool { return false }}
			j, err := openJournal(filepath.Join(t.TempDir(), "rb"))
			if err != nil {
				t.Fatal(err)
			}
			r.j, r.rbDir = j, j.dir
			if err := r.loadImages(context.Background()); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			ok, bad := r.groupByID(r.groupOf["c:good"]), r.groupByID(r.groupOf["c:bad"])
			bad.reason = "没能启动"
			if err := r.commitPartial(context.Background(), []*group{ok}, []*group{bad}); err != errPartial {
				t.Fatal(err)
			}
			if !maps.Equal(d.tags, c.want) {
				t.Fatalf("镜像名 %v，应为 %v", d.tags, c.want)
			}
		})
	}
}

// partialDirs 准备只保留一部分服务时两组的目录：g1 保留、g2 没启动。每组的目录里是迁移过来的
// （new），旁边的备份是原来的（orig）。返回日志里对应的两条记录。
func partialDirs(t *testing.T) (string, []Entry) {
	root := t.TempDir()
	var entries []Entry
	for _, g := range []string{"g1", "g2"} {
		dir, old := filepath.Join(root, g), filepath.Join(root, "."+g+".old")
		for p, s := range map[string]string{dir: "new " + g, old: "orig " + g} {
			os.MkdirAll(p, 0o755)
			os.WriteFile(filepath.Join(p, "f"), []byte(s), 0o644)
		}
		entries = append(entries, Entry{Op: opBind, Path: dir, Existed: true, Backup: old, Group: g})
	}
	return root, entries
}

// checkPartialDone 确认 g1 留着迁移过来的、备份删掉了，g2 放回了原来的。
func checkPartialDone(t *testing.T, root string) {
	t.Helper()
	for dir, want := range map[string]string{"g1": "new g1", "g2": "orig g2"} {
		if b, _ := os.ReadFile(filepath.Join(root, dir, "f")); string(b) != want {
			t.Errorf("%s 里是 %q，应为 %q", dir, b, want)
		}
		if _, err := os.Lstat(filepath.Join(root, "."+dir+".old")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s 的备份应删掉", dir)
		}
	}
}

// partialRun 是只有空的 Docker 的迁入（没有容器和镜像），回滚资料在 dir。
func partialRun(t *testing.T, dir string) *run {
	fake := dockertest.New(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.HasSuffix(req.URL.Path, "/containers/json") || strings.HasSuffix(req.URL.Path, "/images/json") {
			io.WriteString(w, "[]")
			return true
		}
		return false
	})
	dc := fake.Client()
	log, err := logging.Open(filepath.Join(t.TempDir(), "logs"), "in", &logging.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Finish(logging.Failed, true) })
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(io.Discard, false, nil), Prompt: ui.NewPrompter(nil, io.Discard), Log: log,
		Home: t.TempDir(), Getenv: func(string) string { return "" }}, rbDir: dir}
	r.undo = &undo{mountPoint: noMounts, dc: dc, screen: r.Screen, logf: t.Logf, record: func(e Entry) error { return appendJournal(dir, e) },
		fail: func(string) bool { return false }}
	return r
}

// 只保留一部分服务时，删除没启动的组的整个过程中，日志里一直记着保留下来的组：中途被强制结束，
// 下次运行还能接着清理它们的回滚资料（以前先把日志改成只剩没启动的组，被结束就再也找不到了）。
func TestPartialCommitKeepsJournal(t *testing.T) {
	root, entries := partialDirs(t)
	j, err := openJournal(filepath.Join(root, "rb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		j.add(e)
	}
	r := partialRun(t, j.dir)
	r.j = j
	g1 := &group{id: "g1", items: []*citem{{m: bundle.Container{Name: "a"}, c: &docker.Container{}}}}
	g2 := &group{id: "g2", reason: "没能启动", items: []*citem{{m: bundle.Container{Name: "b"}, c: &docker.Container{}}}}
	r.groups = []*group{g1, g2}
	var during []Entry // 删除没启动的组时日志里的内容
	r.undo.fail = func(point string) bool {
		if point == "rollback-data" {
			during, _ = readJournal(r.rbDir)
		}
		return false
	}
	if err := r.commitPartial(context.Background(), []*group{g1}, []*group{g2}); err != errPartial {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(during, func(e Entry) bool { return e.Op == opBind && e.Group == "g1" }) {
		t.Errorf("删除没启动的组时，日志里应还记着保留下来的组：%+v", during)
	}
	checkPartialDone(t, root)
	if _, err := os.Lstat(r.rbDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("做完之后回滚资料目录应删掉")
	}
}

// 只保留一部分服务时被强制结束：下次运行接着删除没启动的组、清理保留下来的组的回滚资料，不再问。
func TestRecoverPartialCommit(t *testing.T) {
	root, entries := partialDirs(t)
	j, err := openJournal(filepath.Join(root, "rb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range append(entries, Entry{Op: opPartialCommit, Names: []string{"g1"}}) {
		j.add(e)
	}
	j.close()
	r := partialRun(t, j.dir)
	if err := r.recoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkPartialDone(t, root)
	if !r.changed() {
		t.Error("接着做完了上次的迁入，这次运行改动过这台服务器（之后取消、失败时结果里不能说没有改动）")
	}
	if _, err := os.Lstat(r.rbDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("做完之后回滚资料目录应删掉")
	}
}

// 上次迁入被强制结束、新服务器上一个容器都没有时（例如替换了一个没人用的同名数据卷），恢复数据卷的
// 临时容器也要有镜像可用：从本机的镜像里挑一个有名字的（以前只从现有容器的镜像里挑）。
func TestRecoverPicksHelperImage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rb")
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.close()
	r := partialRun(t, dir)
	fake := dockertest.New(t, func(w http.ResponseWriter, req *http.Request) bool {
		switch {
		case strings.HasSuffix(req.URL.Path, "/containers/json"):
			io.WriteString(w, "[]")
		case strings.HasSuffix(req.URL.Path, "/images/json"):
			io.WriteString(w, `[{"Id":"sha256:none","RepoTags":["<none>:<none>"]},{"Id":"sha256:abc","RepoTags":["alpine:3.20"]}]`)
		default:
			return false
		}
		return true
	})
	dc := fake.Client()
	r.Docker, r.undo.dc = dc, dc
	if err := r.recoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.undo.vio == nil || r.undo.vio.image != "alpine:3.20" {
		t.Fatalf("应从本机镜像里挑一个有名字的：%+v", r.undo.vio)
	}
}
