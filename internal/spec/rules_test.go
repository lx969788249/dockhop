package spec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker"
)

func TestIsSystemPath(t *testing.T) {
	yes := []string{"/etc/localtime", "/proc/1/fd", "/var/run/docker.sock", "/run/user/1000", "/", "/opt", "/etc/", "/home/user/.docker/run/docker.sock"}
	no := []string{"/opt/app", "/srv/data", "/etc/nginx", "/etc/letsencrypt", "/home/user/data", "/var/lib/mysql", "/runner", "/procfs"}
	for _, p := range yes {
		if !IsSystemPath(p) {
			t.Errorf("%s 应算宿主机系统路径", p)
		}
	}
	for _, p := range no {
		if IsSystemPath(p) {
			t.Errorf("%s 不应算宿主机系统路径", p)
		}
	}
}

func TestPathsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/srv/app", "/srv/app/data", true},
		{"/srv/app/", "/srv/app", true},
		{"/srv/app", "/srv/application", false},
		{"/srv/a", "/srv/b", false},
		{"/", "/x", true},
	}
	for _, c := range cases {
		if got := PathsOverlap(c.a, c.b); got != c.want {
			t.Errorf("PathsOverlap(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestDependencies(t *testing.T) {
	var c docker.Container
	c.HostConfig.NetworkMode = "container:vpn"
	c.HostConfig.PidMode = "host"
	c.HostConfig.IpcMode = "container:0123456789ab"
	c.HostConfig.Links = []string{"/db:/web/database"}
	c.HostConfig.VolumesFrom = []string{"data:ro", "cache"}
	want := []string{"vpn", "0123456789ab", "db", "data", "cache"}
	if got := Dependencies(&c); !reflect.DeepEqual(got, want) {
		t.Fatalf("Dependencies = %v，期望 %v", got, want)
	}
}

func TestLayerChanges(t *testing.T) {
	// 只有父目录因为子项变化被标记、运行时目录、Docker 管理的文件、挂载点：不算改动。
	quiet := []docker.Change{
		{Path: "/var", Kind: 0}, {Path: "/var/run", Kind: 0}, {Path: "/var/run/nginx.pid", Kind: 1},
		{Path: "/tmp/x", Kind: 1}, {Path: "/root/.cache/pip", Kind: 1}, {Path: "/etc", Kind: 0}, {Path: "/etc/hosts", Kind: 0},
		{Path: "/srv", Kind: 1}, {Path: "/srv/data", Kind: 1}, // 新版 Docker 为挂载 /srv/data 创建的目录
	}
	if WritableChanged(quiet, []string{"/srv/data"}) {
		t.Error("只有运行时文件和挂载点时不应算改动")
	}
	keep, deleted := LayerChanges(append(quiet,
		docker.Change{Path: "/etc/app.conf", Kind: 0},
		docker.Change{Path: "/opt/tool", Kind: 1}, docker.Change{Path: "/opt/tool/bin", Kind: 1}, docker.Change{Path: "/opt/tool/bin/x", Kind: 1},
		docker.Change{Path: "/srv/notes.txt", Kind: 1},
		docker.Change{Path: "/usr/share/doc", Kind: 2}, docker.Change{Path: "/usr/share/doc/a", Kind: 2},
	), []string{"/srv/data"})
	if strings.Join(keep, ",") != "/etc/app.conf,/opt/tool,/srv/notes.txt" {
		t.Errorf("新增和修改：%v", keep)
	}
	if strings.Join(deleted, ",") != "/usr/share/doc" {
		t.Errorf("删除：%v", deleted)
	}
	// 镜像里本来没有 /sbin：--init 时 Docker 新建了 /sbin 来放 docker-init（实测 cdt-monitor）。
	if WritableChanged([]docker.Change{{Path: "/sbin", Kind: 1}, {Path: "/sbin/docker-init", Kind: 1}}, nil) {
		t.Error("为 docker-init 新建的 /sbin 不算改动")
	}
	keep, _ = LayerChanges([]docker.Change{{Path: "/sbin", Kind: 1}, {Path: "/sbin/docker-init", Kind: 1}, {Path: "/sbin/mytool", Kind: 1}}, nil)
	if strings.Join(keep, ",") != "/sbin/mytool" {
		t.Errorf("/sbin 里用户自己加的文件仍要迁移：%v", keep)
	}
}

func TestPanelAndSwarm(t *testing.T) {
	var c docker.Container
	c.Config.Image = "moelin/1Panel:latest"
	if !IsPanelContainer(&c) {
		t.Error("1Panel 镜像应识别为面板容器")
	}
	if !IsSwarmTask(map[string]string{"com.docker.swarm.service.id": "x"}) || IsSwarmTask(nil) {
		t.Error("Swarm 标签识别不对")
	}
	if !IsAnonymousVolume("3977206abb71f8aa4677d89b605e71e99ae3db6773688b6f62e61fbbadf9c34c") || IsAnonymousVolume("site_data") {
		t.Error("匿名卷识别不对")
	}
}
