package spec

import (
	"reflect"
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

func TestWritableChanged(t *testing.T) {
	// 只有父目录因为子项变化被标记、以及运行时目录里的改动：不算改动。
	quiet := []docker.Change{
		{Path: "/var", Kind: 0}, {Path: "/var/run", Kind: 0}, {Path: "/var/run/nginx.pid", Kind: 1},
		{Path: "/tmp/x", Kind: 1}, {Path: "/root/.cache/pip", Kind: 1},
	}
	if WritableChanged(quiet) {
		t.Error("只有运行时文件变化时不应算改动")
	}
	real := append(quiet, docker.Change{Path: "/etc/app.conf", Kind: 0})
	if !WritableChanged(real) {
		t.Error("改了 /etc/app.conf 应算改动")
	}
	if !WritableChanged([]docker.Change{{Path: "/usr/local/bin/tool", Kind: 1}}) {
		t.Error("新增文件应算改动")
	}
}

func TestSecretEnvValues(t *testing.T) {
	got := SecretEnvValues([]string{"MYSQL_ROOT_PASSWORD=hunter2", "MODE=prod", "API_KEY=abc", "EMPTY_TOKEN=", "DATABASE_URL=postgres://u:p@db/x"})
	want := []string{"hunter2", "abc", "postgres://u:p@db/x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SecretEnvValues = %v", got)
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
