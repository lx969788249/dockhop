package restore

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/preflight"
)

// 迁入要写到的地方包括 Compose 项目的目录和项目目录外的文件：新服务器上用着它们的已有容器
// 要先停下，保留的服务则不能迁入（不然项目文件会盖掉它正在用的配置）。
func TestWriteSetIncludesProjectFiles(t *testing.T) {
	r := &run{m: &bundle.Manifest{}, projects: []bundle.Project{{Name: "blog", WorkingDir: "/srv/blog", Outside: []string{"/etc/blog.env"}}}}
	vols, dirs := r.writeSet()
	if len(vols) != 0 || fmt.Sprint(dirs) != "[{p:blog /srv/blog} {p:blog /etc/blog.env}]" {
		t.Fatalf("要写的地方：%v %v", vols, dirs)
	}
	c := &docker.Container{Mounts: []docker.Mount{{Type: "bind", Source: "/srv/blog/config"}}}
	if !usesData(c, vols, dirs) {
		t.Error("挂载了项目目录里的子目录，也算在用要写入的数据")
	}
}

// 和迁入的数据共用的已有容器要先停下：用 --rm 启动的一停就被 Docker 删掉，Swarm 的任务停了会
// 被换成新的，都恢复不了。要在改动任何东西之前停下，也不能去停它。
func TestDecideRefusesUnstoppableWriters(t *testing.T) {
	for _, c := range []struct{ name, extra, want string }{
		{"--rm", `"HostConfig":{"AutoRemove":true},"Config":{"Labels":{}}`, "是用 --rm 启动的"},
		{"Swarm", `"HostConfig":{},"Config":{"Labels":{"com.docker.swarm.service.id":"x","com.docker.swarm.service.name":"web"}}`,
			"属于 Swarm 服务 web"},
	} {
		failed, stopped := prepareWith(t, preflight.Decisions{}, func(w http.ResponseWriter, p, shared string) bool {
			switch p {
			case "/containers/json":
				io.WriteString(w, `[{"Id":"other-id","Names":["/other"],"State":"running"}]`)
			case "/containers/other-id/json":
				io.WriteString(w, `{"Id":"other-id","Name":"/other","State":{"Status":"running","Running":true},`+c.extra+
					`,"Mounts":[{"Type":"bind","Source":"`+shared+`"}]}`)
			default:
				return false
			}
			return true
		})
		if !strings.Contains(failed, c.want) || !strings.Contains(failed, "还没有改动任何东西") || len(stopped) > 0 {
			t.Errorf("%s：应在改动之前停下：%q，停过 %v", c.name, failed, stopped)
		}
	}
}
