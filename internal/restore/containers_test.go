package restore

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 同一个 Compose 服务的副本只能用同一个镜像：镜像里只删每个副本都删过的文件，只有部分副本删过
// 的明确提示会重新出现；新增和修改的文件各写各的，不放进镜像。
func TestServiceImageKeepsReplicasApart(t *testing.T) {
	rep := func(name string, deleted ...string) *citem {
		c := &docker.Container{}
		c.Config.Image = "app:1"
		return &citem{m: bundle.Container{Name: name, Changes: &bundle.Changes{Deleted: deleted}}, c: c}
	}
	a, b := rep("stack-web-1", "/etc/a", "/etc/common"), rep("stack-web-2", "/etc/common", "/etc/b")
	var out bytes.Buffer
	r := testRun(t, Deps{Screen: ui.New(&out, false, nil)}, Options{})
	r.vio = &volumeIO{}
	img, err := r.serviceImage(context.Background(), "stack", "web", []*citem{a, b})
	if err != nil {
		t.Fatal(err)
	}
	// 这里没有能删除文件的程序（Docker 不在本机）：照原镜像，并且说明。
	if img != "app:1" || a.img != img || b.img != img {
		t.Errorf("镜像：%q（%q、%q）", img, a.img, b.img)
	}
	for _, w := range []string{"容器 stack-web-1 里删除过的 1 个文件在新服务器上会重新出现", "容器 stack-web-2 里删除过的 1 个文件"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("应提示「%s」：\n%s", w, out.String())
		}
	}
	if got := intersect([]string{"/a", "/b", "/c"}, []string{"/c", "/a"}); strings.Join(got, ",") != "/a,/c" {
		t.Errorf("每个副本都删过的：%v", got)
	}
}

// 副本按编号排好，才能和新服务器上的容器一一对应（编号可能不连续）。
func TestReplicasOrder(t *testing.T) {
	mk := func(name, svc, num string) *citem {
		c := &docker.Container{}
		c.Config.Labels = map[string]string{"com.docker.compose.container-number": num}
		return &citem{m: bundle.Container{Name: name, Service: svc}, c: c}
	}
	items := []*citem{mk("p-web-3", "web", "3"), mk("p-db-1", "db", "1"), mk("p-web-1", "web", "1"), mk("p-web-10", "web", "10")}
	var got []string
	for _, it := range replicas(items, "web") {
		got = append(got, it.m.Name)
	}
	if strings.Join(got, ",") != "p-web-1,p-web-3,p-web-10" {
		t.Errorf("副本顺序：%v", got)
	}
}
