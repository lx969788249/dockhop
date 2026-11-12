package preflight

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker/dockertest"
)

// 读新服务器的情况：Docker 的概况，已有的容器（所在的 Compose 项目、状态、用到的数据卷和目录、对外开放的
// 端口）、数据卷、网络（网段和每个容器的 IP）；读不到的数据卷、网络跳过。
func TestCollect(t *testing.T) {
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/info":
			io.WriteString(w, `{"Architecture":"x86_64","ServerVersion":"27.5.1","MemTotal":1024,"Runtimes":{"runc":{},"nvidia":{}},`+
				`"Plugins":{"Volume":["local","rclone"]}}`)
		case "/containers/json":
			io.WriteString(w, `[{"Id":"c1","Names":["/web"],"State":"running","Labels":{"com.docker.compose.project":"site"},`+
				`"Mounts":[{"Type":"volume","Name":"data"},{"Type":"bind","Source":"/srv/web"}],`+
				`"Ports":[{"Type":"tcp","IP":"0.0.0.0","PublicPort":8080,"PrivatePort":80},{"Type":"tcp","PrivatePort":9000}]}]`)
		case "/volumes":
			io.WriteString(w, `{"Volumes":[{"Name":"data"},{"Name":"gone"}]}`)
		case "/volumes/data":
			io.WriteString(w, `{"Name":"data","Driver":"local"}`)
		case "/networks":
			io.WriteString(w, `[{"Id":"n1","Name":"site_default"},{"Id":"n2","Name":"broken"}]`)
		case "/networks/n1":
			io.WriteString(w, `{"Name":"site_default","Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.20.0.0/16"},{"Gateway":"172.20.0.1"}]},`+
				`"Containers":{"c1":{"Name":"web","IPv4Address":"172.20.0.2/16"},"c3":{"Name":"noip","IPv4Address":""}}}`)
		default:
			return false // 读不到 /volumes/gone、/networks/n2
		}
		return true
	})
	got, err := Collect(context.Background(), fake.Client(), "2.29.7")
	if err != nil {
		t.Fatal(err)
	}
	type view struct {
		Arch, DockerVersion, DockerRoot, Compose string
		MemTotal                                 int64
		HasCompose, Local                        bool
		Runtimes, VolumePlugins, HostIPs         map[string]bool
		Containers                               map[string]TargetContainer
		DockerPorts                              []DockerPort
		VolumeDrivers                            map[string]string
		Networks                                 map[string]TargetNetwork
	}
	g := view{got.Arch, got.DockerVersion, got.DockerRoot, got.Compose, got.MemTotal, got.HasCompose, got.Local,
		got.Runtimes, got.VolumePlugins, got.HostIPs, got.Containers, got.DockerPorts, map[string]string{}, got.Networks}
	for name, v := range got.Volumes {
		g.VolumeDrivers[name] = v.Driver
	}
	want := view{Arch: "x86_64", DockerVersion: "27.5.1", DockerRoot: "/var/lib/docker", Compose: "2.29.7", MemTotal: 1024, HasCompose: true,
		Runtimes:      map[string]bool{"runc": true, "nvidia": true},
		VolumePlugins: map[string]bool{"local": true, "rclone": true},
		Containers:    map[string]TargetContainer{"web": {Project: "site", State: "running", Volumes: []string{"data"}, Binds: []string{"/srv/web"}}},
		DockerPorts:   []DockerPort{{Proto: "tcp", IP: "0.0.0.0", Port: "8080", Container: "web"}},
		VolumeDrivers: map[string]string{"data": "local"},
		Networks:      map[string]TargetNetwork{"site_default": {Driver: "bridge", Subnets: []string{"172.20.0.0/16"}, IPs: map[string]string{"172.20.0.2": "web"}}},
	}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("读到的：\n%+v\n应为：\n%+v", g, want)
	}
}
