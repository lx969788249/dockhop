package restore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
)

// oldContainer 按 docker inspect 的样子造一个旧服务器上的容器。
func oldContainer(t *testing.T, raw string) *citem {
	t.Helper()
	var c docker.Container
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return &citem{m: bundle.Container{Name: c.ShortName(), Project: c.Config.Labels["com.docker.compose.project"],
		Service: c.Config.Labels["com.docker.compose.service"]}, c: &c, raw: json.RawMessage(raw)}
}

// 1Panel 装的应用是各自的 Compose 项目，接在同一个 1panel-network 上。Halo 的启动参数里写着
// 数据库的服务名 postgresql：PostgreSQL 要先启动，等它就绪后再启动 Halo；Nginx Proxy Manager
// 和它们没关系，不用等。
func TestStartOrderFollowsReferences(t *testing.T) {
	halo := oldContainer(t, `{"Name":"/halo","Config":{"Labels":{"com.docker.compose.project":"halo","com.docker.compose.service":"halo"},
		"Env":["JVM_OPTS=","PATH=/usr/local/sbin:/usr/local/bin"],
		"Cmd":["--spring.r2dbc.url=r2dbc:pool:postgresql://postgresql:5432/halo","--spring.r2dbc.password=secret-pass","--spring.sql.init.platform=postgresql"]},
		"NetworkSettings":{"Networks":{"1panel-network":{"Aliases":["halo"],"DNSNames":["halo","0123456789ab"]}}}}`)
	npm := oldContainer(t, `{"Name":"/1Panel-nginx-proxy-manager-Xy12","Config":{"Labels":{"com.docker.compose.project":"nginx-proxy-manager","com.docker.compose.service":"nginx-proxy-manager"},
		"Env":["DB_SQLITE_FILE=/data/database.sqlite"],"Cmd":null},
		"NetworkSettings":{"Networks":{"1panel-network":{"Aliases":["nginx-proxy-manager"]}}}}`)
	pg := oldContainer(t, `{"Name":"/1Panel-postgresql-Ab12","Config":{"Labels":{"com.docker.compose.project":"postgresql","com.docker.compose.service":"postgresql"},
		"Env":["POSTGRES_USER=user_ab12","POSTGRES_PASSWORD=pg-pass","PGDATA=/var/lib/postgresql/18/docker"],"Cmd":["postgres"]},
		"NetworkSettings":{"Networks":{"1panel-network":{"Aliases":["postgresql","1Panel-postgresql-Ab12"],"DNSNames":["1Panel-postgresql-Ab12","postgresql","fedcba987654"]}}}}`)
	// 独立容器：环境变量里写着 Halo 的地址，但不在同一个网络上，按名字本来就连不上，不算。
	probe := oldContainer(t, `{"Name":"/probe","Config":{"Env":["TARGET=http://halo:8090"]},"NetworkSettings":{"Networks":{"bridge":{}}}}`)
	// 独立容器：环境变量里写着数据库的容器名（大小写不同也算）。
	backup := oldContainer(t, `{"Name":"/pg-backup","Config":{"Env":["PGHOST=1panel-postgresql-ab12"]},
		"NetworkSettings":{"Networks":{"1panel-network":{"Aliases":null}}}}`)

	projects := []bundle.Project{{Name: "halo"}, {Name: "nginx-proxy-manager"}, {Name: "postgresql"}}
	r := testMigrate(t, Deps{}, Options{})
	r.sc.items = []*citem{halo, npm, pg, probe, backup}
	r.sc.projects = projects
	r.pkg.m = &bundle.Manifest{}
	units := r.units(&group{projects: projects, items: r.sc.items})
	deps := depsOf(units)
	want := map[string]string{
		"p:halo":                "p:postgresql 最好先就绪（halo 的启动命令里写着 postgresql）",
		"p:nginx-proxy-manager": "",
		"p:postgresql":          "",
		"c:probe":               "",
		"c:pg-backup":           "p:postgresql 最好先就绪（pg-backup 的环境变量 PGHOST 里写着 1panel-postgresql-ab12）",
	}
	for k, w := range want {
		if deps[k] != w {
			t.Errorf("%s 用到：%q，应为 %q", k, deps[k], w)
		}
	}
	if got := orderOf(units, (*unit).startAfter); got != "p:nginx-proxy-manager p:postgresql c:probe c:pg-backup p:halo" {
		t.Errorf("启动顺序：%s", got)
	}
	// 猜出来的依赖不影响创建的先后。
	if got := orderOf(units, (*unit).createAfter); got != "p:halo p:nginx-proxy-manager p:postgresql c:probe c:pg-backup" {
		t.Errorf("创建顺序：%s", got)
	}
	if why := deps["p:halo"]; strings.Contains(why, "secret-pass") {
		t.Errorf("理由里不应有启动参数的值：%s", why)
	}
}

// 同一个 Compose 项目里，asidecar 借用 vpn 的网络（network_mode: service:vpn，容器上记的是
// container:<vpn 的 ID>）。新服务器只有老版 docker-compose、读不出 depends_on 时，也要先启动 vpn。
func TestServiceOrderFollowsContainerReferences(t *testing.T) {
	vpn := oldContainer(t, `{"Name":"/stack-vpn-1","Config":{"Labels":{"com.docker.compose.project":"stack","com.docker.compose.service":"vpn"}}}`)
	side := oldContainer(t, `{"Name":"/stack-asidecar-1","Config":{"Labels":{"com.docker.compose.project":"stack","com.docker.compose.service":"asidecar"}},
		"HostConfig":{"NetworkMode":"container:old-vpn"}}`)
	projects := []bundle.Project{{Name: "stack"}}
	r := testMigrate(t, Deps{}, Options{})
	r.sc.items = []*citem{side, vpn}
	r.sc.projects = projects
	r.pkg.m = &bundle.Manifest{Containers: []bundle.Container{{Name: "stack-asidecar-1"}, {Name: "stack-vpn-1", ID: "old-vpn"}}}
	u := r.units(&group{projects: projects, items: r.sc.items})[0]
	u.services = uniqueServices(u.items) // 创建项目时才有（见 createProject）；读不出配置时 configured 为空
	if got := strings.Join(u.serviceOrder(), " "); got != "vpn asidecar" {
		t.Fatalf("启动顺序：%s", got)
	}
}

// 要先存在的必须先创建：项目 aapp 用的网络是项目 zproxy 建的（Compose 按 zproxy 的文件建它），
// 项目 bweb 借用独立容器 vpn 的网络，独立容器 sidecar 用 --volumes-from 了 data。借用网络还要求
// 先就绪才能启动；只要先存在的，启动不用等。
func TestCreateOrderFollowsExistence(t *testing.T) {
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, "networks"), 0o700)
	os.WriteFile(filepath.Join(data, "networks", "proxynet.json"),
		[]byte(`{"Name":"proxynet","Labels":{"com.docker.compose.project":"zproxy","com.docker.compose.network":"proxy"}}`), 0o600)
	app := oldContainer(t, `{"Name":"/aapp-app-1","Config":{"Labels":{"com.docker.compose.project":"aapp","com.docker.compose.service":"app"}},
		"NetworkSettings":{"Networks":{"proxynet":{}}}}`)
	proxy := oldContainer(t, `{"Name":"/zproxy-proxy-1","Config":{"Labels":{"com.docker.compose.project":"zproxy","com.docker.compose.service":"proxy"}},
		"NetworkSettings":{"Networks":{"proxynet":{}}}}`)
	web := oldContainer(t, `{"Name":"/bweb-web-1","Config":{"Labels":{"com.docker.compose.project":"bweb","com.docker.compose.service":"web"}},
		"HostConfig":{"NetworkMode":"container:vpn"}}`)
	vpn := oldContainer(t, `{"Name":"/vpn","NetworkSettings":{"Networks":{"bridge":{}}}}`)
	sidecar := oldContainer(t, `{"Name":"/sidecar","HostConfig":{"VolumesFrom":["data:ro"]}}`)
	store := oldContainer(t, `{"Name":"/data"}`)
	projects := []bundle.Project{{Name: "aapp"}, {Name: "bweb"}, {Name: "zproxy"}}
	r := testMigrate(t, Deps{}, Options{})
	r.sc.items = []*citem{app, proxy, web, sidecar, store, vpn}
	r.sc.projects = projects
	r.pkg.dataDir = data
	r.pkg.m = &bundle.Manifest{Networks: []string{"proxynet"}}
	units := r.units(&group{projects: projects, items: r.sc.items})
	deps := depsOf(units)
	want := map[string]string{
		"p:aapp":    "p:zproxy 要先存在（aapp-app-1 接在 Compose 项目 zproxy 建的网络 proxynet 上）",
		"p:bweb":    "c:vpn 要先就绪（bweb-web-1 用借用网络（或 --link）连着 vpn）",
		"c:sidecar": "c:data 要先存在（sidecar 用--volumes-from连着 data）",
	}
	for k, w := range want {
		if deps[k] != w {
			t.Errorf("%s 用到：%q，应为 %q", k, deps[k], w)
		}
	}
	if got := orderOf(units, (*unit).createAfter); got != "p:zproxy c:data c:vpn p:aapp p:bweb c:sidecar" {
		t.Errorf("创建顺序：%s", got)
	}
	if got := orderOf(units, (*unit).startAfter); got != "p:aapp p:zproxy c:sidecar c:data c:vpn p:bweb" {
		t.Errorf("启动顺序：%s", got)
	}
}

func depsOf(units []*unit) map[string]string {
	out := map[string]string{}
	for _, u := range units {
		var parts []string
		for _, d := range u.deps {
			kind := "要先存在"
			switch {
			case d.ready:
				kind = "要先就绪"
			case d.prefer:
				kind = "最好先就绪"
			}
			parts = append(parts, d.on.key+" "+kind+"（"+strings.Join(d.why, "；")+"）")
		}
		out[u.key] = strings.Join(parts, "；")
	}
	return out
}

func orderOf(units []*unit, after func(*unit) []string) string {
	var keys []string
	for _, u := range dependencyOrder(units, unitKey, after) {
		keys = append(keys, u.key)
	}
	return strings.Join(keys, " ")
}

func TestDependencyOrder(t *testing.T) {
	mk := func(name, mode string, links ...string) *citem {
		c := &docker.Container{Name: "/" + name}
		c.HostConfig.NetworkMode = mode
		c.HostConfig.VolumesFrom = links
		return &citem{m: bundle.Container{Name: name}, c: c}
	}
	items := []*citem{mk("side", "container:abc123abc123"), mk("app", "bridge", "data"), mk("data", "bridge"), mk("web", "bridge")}
	nameOf := func(ref string) string {
		if strings.HasPrefix("abc123abc123ffff", ref) {
			return "web"
		}
		return ""
	}
	var got []string
	for _, it := range dependencyOrder(items, (*citem).name, func(it *citem) []string { return containerDeps(it.c, nameOf) }) {
		got = append(got, it.m.Name)
	}
	pos := map[string]int{}
	for i, n := range got {
		pos[n] = i
	}
	if pos["web"] > pos["side"] || pos["data"] > pos["app"] || len(got) != 4 {
		t.Errorf("被依赖的容器应先创建：%v", got)
	}
	// 循环依赖时不卡住。
	a, b := mk("a", "container:b"), mk("b", "container:a")
	if n := len(dependencyOrder([]*citem{a, b}, (*citem).name, func(it *citem) []string { return containerDeps(it.c, func(string) string { return "" }) })); n != 2 {
		t.Errorf("循环依赖：%d", n)
	}
}
