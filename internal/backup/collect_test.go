package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/ui"
)

func TestDriftReason(t *testing.T) {
	str := func(s string) *string { return &s }
	var c docker.Container
	c.Config.Image = "nginx:1.27"
	c.Config.Env = []string{"PATH=/usr/bin", "MODE=prod", "FROM_FILE=yes"}
	same := compose.Service{Image: "nginx:1.27", Environment: map[string]*string{"MODE": str("prod"), "FROM_FILE": str("yes"), "PASSTHROUGH": nil}}
	if why := driftReason(same, &c); why != "" {
		t.Errorf("没有改动时不应报告：%s", why)
	}
	changed := compose.Service{Image: "nginx:1.27", Environment: map[string]*string{"MODE": str("dev"), "NEW": str("1")}}
	if why := driftReason(changed, &c); why != "环境变量 MODE、NEW" {
		t.Errorf("环境变量不同应报告：%q", why)
	}
	if why := driftReason(compose.Service{Image: "nginx:1.29"}, &c); why != "镜像 nginx:1.27 → nginx:1.29" {
		t.Errorf("镜像不同应报告：%q", why)
	}
}

// 菜单里不能选的容器也要算进共用数据的检查：docker compose run 的临时容器正在写选中的
// 容器的卷时，打包时要一起临时停止；Swarm 服务的容器停不住（会被马上重新创建），只能取消。
func TestSharedWritersIncludeUnlisted(t *testing.T) {
	for label, want := range map[string]string{`"com.docker.compose.oneoff":"True"`: "停止", `"com.docker.swarm.service.id":"s1","com.docker.swarm.service.name":"jobs"`: "取消"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch strings.TrimPrefix(r.URL.Path, "/v1.44") {
			case "/version":
				io.WriteString(w, `{"Version":"29.0.0","ApiVersion":"1.44"}`)
			case "/containers/json":
				io.WriteString(w, `[{"Id":"web-id","Names":["/web"]},{"Id":"writer-id","Names":["/writer"]}]`)
			case "/containers/web-id/json":
				io.WriteString(w, `{"Id":"web-id","Name":"/web","Image":"sha256:img","Config":{"Image":"alpine"},"State":{"Running":true},"Mounts":[{"Type":"volume","Name":"data","Destination":"/data","RW":true}]}`)
			case "/containers/writer-id/json":
				io.WriteString(w, `{"Id":"writer-id","Name":"/writer","Image":"sha256:img","Config":{"Image":"alpine","Labels":{`+label+`}},"State":{"Running":true},"Mounts":[{"Type":"volume","Name":"data","Destination":"/data","RW":true}]}`)
			case "/system/df":
				io.WriteString(w, `{"Volumes":[{"Name":"data","UsageData":{"Size":10}}]}`)
			case "/images/sha256:img/json":
				io.WriteString(w, `{"Id":"sha256:img","Size":100}`)
			case "/volumes/data":
				io.WriteString(w, `{"Name":"data","Driver":"local"}`)
			default:
				http.NotFound(w, r)
			}
		}))
		dc, err := docker.New(context.Background(), docker.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")))
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		s := ui.New(&out, false, nil)
		inv, err := buildInventory(context.Background(), dc, s)
		if err != nil {
			t.Fatal(err)
		}
		col, err := collect(context.Background(), dc, nil, inv, &selection{units: inv.units, items: inv.units[0].items}, s, nil)
		switch {
		case want == "停止" && (err != nil || len(col.shared) != 1 || col.shared[0].name() != "writer"):
			t.Errorf("临时容器应一起停止：%v", err)
		case want == "取消" && (err == nil || s.Failure() == nil || !strings.Contains(s.Failure().Hint, "docker service scale jobs=0")):
			t.Errorf("Swarm 服务的容器应取消：%v\n%s", err, out.String())
		}
		srv.Close()
	}
}

// 打包 Compose 项目目录和项目目录外的文件时，没选中、正在运行、挂载着它们的容器也要一起临时停止：
// 以前只比较数据卷和挂载目录，它们在打包的时候还在写，迁过去的数据前后对不上。
func TestSharedWritersIncludeProjectFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "app")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services:\n  web:\n    image: alpine\n    env_file: ../shared.env\n"), 0o644)
	os.WriteFile(filepath.Join(root, "shared.env"), []byte("A=1\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "elsewhere"), 0o755)
	labels := `{"com.docker.compose.project":"app","com.docker.compose.service":"web","com.docker.compose.project.working_dir":` +
		strconv.Quote(dir) + `,"com.docker.compose.project.config_files":` + strconv.Quote(filepath.Join(dir, "compose.yml")) + `}`
	bind := func(src string) string {
		return `"Mounts":[{"Type":"bind","Source":` + strconv.Quote(src) + `,"Destination":"/x","RW":true}]`
	}
	containers := map[string]string{
		"web":       `"Config":{"Image":"alpine","Labels":` + labels + `}`,
		"sidecar":   `"Config":{"Image":"alpine"},` + bind(dir),
		"envreader": `"Config":{"Image":"alpine"},` + bind(filepath.Join(root, "shared.env")),
		"other":     `"Config":{"Image":"alpine"},` + bind(filepath.Join(root, "elsewhere")),
		// 只读地挂着项目的上级目录（例如监控）：改不了数据，不用停。
		"monitor": `"Config":{"Image":"alpine"},"Mounts":[{"Type":"bind","Source":` + strconv.Quote(root) + `,"Destination":"/host","RW":false}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v1.44")
		switch {
		case p == "/version":
			io.WriteString(w, `{"Version":"29.0.0","ApiVersion":"1.44"}`)
		case p == "/containers/json":
			var list []string
			for name := range containers {
				list = append(list, `{"Id":"`+name+`-id","Names":["/`+name+`"]}`)
			}
			io.WriteString(w, "["+strings.Join(list, ",")+"]")
		case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "-id/json"):
			name := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "-id/json")
			io.WriteString(w, `{"Id":"`+name+`-id","Name":"/`+name+`","Image":"sha256:img","State":{"Running":true},`+containers[name]+`}`)
		case p == "/system/df":
			io.WriteString(w, `{}`)
		case p == "/images/sha256:img/json":
			io.WriteString(w, `{"Id":"sha256:img","Size":100}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dc, err := docker.New(context.Background(), docker.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s := ui.New(&out, false, nil)
	inv, err := buildInventory(context.Background(), dc, s)
	if err != nil {
		t.Fatal(err)
	}
	var app *unit
	for _, u := range inv.units {
		if u.project == "app" {
			app = u
		}
	}
	if app == nil {
		t.Fatalf("没认出 Compose 项目：%+v", inv.units)
	}
	col, err := collect(context.Background(), dc, nil, inv, &selection{units: []*unit{app}, items: app.items}, s, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var names []string
	for _, it := range col.shared {
		names = append(names, it.name())
	}
	slices.Sort(names)
	if got := strings.Join(names, " "); got != "envreader sidecar" {
		t.Errorf("要一起停的：%q，应为挂载着项目目录和项目外的文件的 envreader sidecar", got)
	}
}

// 从读容器到停机，中间可能隔了好一会儿（选菜单、回答问题）。这期间和这次迁出有关的容器启动、停下、
// 暂停、被重建，或者新建了共用数据的容器，就不能按读的时候的样子停机打包：新启动的共用数据的容器
// 没停下，打包时它还在写；已经停下的容器打包完会被启动起来。停机之前再看一次，有变化就停下，什么都
// 不动；和这次迁出无关的容器怎么变都不管。
func TestRecheckBeforeStop(t *testing.T) {
	vol := `,"Mounts":[{"Type":"volume","Name":"data","Destination":"/data","RW":true}]`
	type ctr struct{ id, state, mounts string }
	before := map[string]ctr{
		"web":    {"web-id", "running", vol},
		"writer": {"writer-id", "exited", vol},
		"reader": {"reader-id", "running", vol},
		"other":  {"other-id", "running", ""},
	}
	for _, c := range []struct {
		name   string
		change func(map[string]ctr)
		want   string // 报错里要提到的容器；为空表示可以停机
	}{
		{"没有变化", func(map[string]ctr) {}, ""},
		{"无关的容器停了", func(m map[string]ctr) { m["other"] = ctr{"other-id", "exited", ""} }, ""},
		{"共用数据的容器启动了", func(m map[string]ctr) { m["writer"] = ctr{"writer-id", "running", vol} }, "writer"},
		{"共用数据的容器停了", func(m map[string]ctr) { m["reader"] = ctr{"reader-id", "exited", vol} }, "reader"},
		{"选中的容器停了", func(m map[string]ctr) { m["web"] = ctr{"web-id", "exited", vol} }, "web"},
		{"选中的容器暂停了", func(m map[string]ctr) { m["web"] = ctr{"web-id", "paused", vol} }, "web"},
		{"选中的容器被重建了", func(m map[string]ctr) { m["web"] = ctr{"web-new", "running", vol} }, "web"},
		{"新建了共用数据的容器", func(m map[string]ctr) { m["late"] = ctr{"late-id", "running", vol} }, "late"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			cur := maps.Clone(before)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				p := strings.TrimPrefix(r.URL.Path, "/v1.44")
				switch {
				case p == "/version":
					io.WriteString(w, `{"Version":"29.0.0","ApiVersion":"1.44"}`)
				case p == "/containers/json":
					var list []string
					for name, x := range cur {
						list = append(list, `{"Id":"`+x.id+`","Names":["/`+name+`"],"State":"`+x.state+`"}`)
					}
					io.WriteString(w, "["+strings.Join(list, ",")+"]")
				case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
					id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
					for name, x := range cur {
						if x.id == id {
							running := x.state == "running" || x.state == "paused"
							fmt.Fprintf(w, `{"Id":%q,"Name":"/%s","Image":"sha256:img","Config":{"Image":"alpine"},"State":{"Status":%q,"Running":%t,"Paused":%t}%s}`,
								x.id, name, x.state, running, x.state == "paused", x.mounts)
							return
						}
					}
					http.NotFound(w, r)
				case p == "/system/df":
					io.WriteString(w, `{"Volumes":[{"Name":"data","UsageData":{"Size":10}}]}`)
				case p == "/images/sha256:img/json":
					io.WriteString(w, `{"Id":"sha256:img","Size":100}`)
				case p == "/volumes/data":
					io.WriteString(w, `{"Name":"data","Driver":"local"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			dc, err := docker.New(context.Background(), docker.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")))
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			s := ui.New(&out, false, nil)
			inv, err := buildInventory(context.Background(), dc, s)
			if err != nil {
				t.Fatal(err)
			}
			var web *unit
			for _, u := range inv.units {
				if u.items[0].name() == "web" {
					web = u
				}
			}
			col, err := collect(context.Background(), dc, nil, inv, &selection{units: []*unit{web}, items: web.items}, s, nil)
			if err != nil || len(col.shared) != 1 || col.shared[0].name() != "reader" {
				t.Fatalf("读的时候要一起停的应是 reader：%v %v", col.shared, err)
			}
			mu.Lock()
			c.change(cur)
			mu.Unlock()
			r := &run{Deps: Deps{Docker: dc, Screen: s, Getenv: func(string) string { return "" }}}
			err = r.recheck(context.Background(), col)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("没有相关的变化，应能停机：%v\n%s", err, out.String())
			case c.want != "" && (err == nil || s.Failure() == nil || !strings.Contains(s.Failure().Reason, c.want)):
				t.Errorf("应停下并说出 %s：%v\n%s", c.want, err, out.String())
			}
		})
	}
}
