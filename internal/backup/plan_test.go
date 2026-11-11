package backup

import (
	"reflect"
	"slices"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
)

// 迁移计划记下每个容器挂载的是迁移包里的哪个目录：挂载的路径就是它、在它里面，或者是指向它的软链接，都要
// 算上；还要记下 Compose 项目的目录和项目目录外的文件。迁入前检查和迁入都按迁移计划算要写到新服务器上的
// 哪些地方（见 preflight.WritesOf），以前挂载的路径在迁移的目录里面、经过软链接的都没记。
func TestBuildPlanRecordsWrites(t *testing.T) {
	bind := func(name string, sources ...string) *item {
		c := &docker.Container{Name: "/" + name}
		for _, s := range sources {
			c.Mounts = append(c.Mounts, docker.Mount{Type: "bind", Source: s})
		}
		return &item{c: c}
	}
	web, logs, other := bind("web", "/srv/app/data"), bind("logs", "/data/app/logs"), bind("other", "/srv/apple")
	col := &collected{
		sel: &selection{items: []*item{web, logs, other},
			units: []*unit{{project: "site", workingDir: "/srv/app/compose", items: []*item{web}}}},
		binds: []*bindInfo{{path: "/srv/app", links: []bundle.BindLink{{Link: "/data/app", Target: "/srv/app"}}}},
	}
	m := &bundle.Manifest{Projects: []bundle.Project{{Name: "site", WorkingDir: "/srv/app/compose", Outside: []string{"/etc/site.env"}}}}
	p := buildPlan(m, col, 1, "sum")
	if p.Version != bundle.PlanVersion {
		t.Errorf("版本：%d", p.Version)
	}
	got := map[string][]string{}
	for _, c := range p.Containers {
		got[c.Name] = c.Binds
	}
	if want := map[string][]string{"web": {"/srv/app"}, "logs": {"/srv/app"}, "other": nil}; !reflect.DeepEqual(got, want) {
		t.Errorf("容器挂载的目录：%v，应为 %v", got, want)
	}
	if len(p.Projects) != 1 || p.Projects[0].WorkingDir != "/srv/app/compose" || !slices.Equal(p.Projects[0].Outside, []string{"/etc/site.env"}) {
		t.Errorf("Compose 项目：%+v", p.Projects)
	}
}
