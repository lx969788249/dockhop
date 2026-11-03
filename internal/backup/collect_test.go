package backup

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
				io.WriteString(w, `{"Id":"web-id","Name":"/web","Image":"sha256:img","Config":{"Image":"alpine"},"State":{"Running":true},"Mounts":[{"Type":"volume","Name":"data","Destination":"/data"}]}`)
			case "/containers/writer-id/json":
				io.WriteString(w, `{"Id":"writer-id","Name":"/writer","Image":"sha256:img","Config":{"Image":"alpine","Labels":{`+label+`}},"State":{"Running":true},"Mounts":[{"Type":"volume","Name":"data","Destination":"/data"}]}`)
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
