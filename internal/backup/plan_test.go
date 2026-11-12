package backup

import (
	"encoding/json"
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

// 迁移计划里记下每个容器要新服务器满足的条件：运行时、显卡（--gpus 或者 nvidia 的设备驱动）、设备、映射到
// 宿主机的端口（Docker 随机分配的不记，没写协议的算 tcp）、接的网络和用户指定的固定 IP、用到的数据卷。
func TestBuildPlanContainerNeeds(t *testing.T) {
	mk := func(js string) *item {
		var c docker.Container
		if err := json.Unmarshal([]byte(js), &c); err != nil {
			t.Fatal(err)
		}
		return &item{c: &c}
	}
	gpu := mk(`{"Name":"/gpu","State":{"Status":"running","Paused":true},"Config":{"Image":"app@sha256:abc"},
		"HostConfig":{"NetworkMode":"bridge","Memory":512,"Runtime":"nvidia",
			"DeviceRequests":[{"Capabilities":[["compute","gpu"]]}],"Devices":[{"PathOnHost":"/dev/dri"}],
			"PortBindings":{"80/tcp":[{"HostIP":"","HostPort":"8080"},{"HostPort":""}],"53":[{"HostIP":"127.0.0.1","HostPort":"5353"}]}},
		"NetworkSettings":{"Networks":{"b":{"IPAMConfig":{"IPv4Address":"172.20.0.5"}},"a":{}}},
		"Mounts":[{"Type":"volume","Name":"data"},{"Type":"bind","Source":"/srv/x"}]}`)
	driver := mk(`{"Name":"/driver","State":{"Status":"exited"},"Config":{"Image":"cuda"},
		"HostConfig":{"Runtime":"runc","DeviceRequests":[{"Driver":"nvidia"}]}}`)
	p := buildPlan(&bundle.Manifest{}, &collected{sel: &selection{items: []*item{gpu, driver}}}, 1, "sum")
	want := []bundle.PlanContainer{
		{Name: "gpu", State: "paused", Image: "app@sha256:abc", DigestPinned: true, NetworkMode: "bridge", Memory: 512,
			Runtime: "nvidia", GPU: true, Devices: []string{"/dev/dri"},
			Ports: []bundle.PlanPort{{HostIP: "127.0.0.1", HostPort: "5353", ContainerPort: "53", Proto: "tcp"},
				{HostPort: "8080", ContainerPort: "80", Proto: "tcp"}},
			Networks: []bundle.PlanNetAddr{{Name: "a"}, {Name: "b", IP: "172.20.0.5"}}, Volumes: []string{"data"}},
		{Name: "driver", State: "exited", Image: "cuda", GPU: true},
	}
	if !reflect.DeepEqual(p.Containers, want) {
		t.Errorf("容器要的条件：\n%+v\n应为：\n%+v", p.Containers, want)
	}
}
