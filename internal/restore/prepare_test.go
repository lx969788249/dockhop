package restore

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 读不到已有的 Compose 容器里有没有改动时，不能当作没有改动、直接删掉它。
func TestBackupProjectStopsWhenChangesFail(t *testing.T) {
	dc, calls := faultyDocker(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/changes"):
			return false
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			w.Write([]byte(`{"Id":"abcdef1234567890","Name":"/blog-app-1","State":{"Status":"running","Running":true},"Config":{"Image":"alpine"}}`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		default:
			return false
		}
		return true
	})
	var out bytes.Buffer
	j, err := openJournal(filepath.Join(t.TempDir(), "rb"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(&out, false, nil)}, j: j}
	err = r.backupProject(context.Background(), "blog", []docker.ContainerSummary{{ID: "abcdef1234567890", Names: []string{"/blog-app-1"}}})
	if err == nil || !strings.Contains(out.String(), "为了安全没有删除它") {
		t.Fatalf("应停下：%v\n%s", err, out.String())
	}
	for _, c := range calls() {
		if strings.HasPrefix(c, "DELETE") {
			t.Fatalf("不应删除旧容器：%s", c)
		}
	}
}

// 选择保留的已有容器（停着的也算）和要迁入的容器共用目录：不能覆盖它的数据，改动之前停下。
func TestPrepareProtectsKeptData(t *testing.T) {
	failed, stopped := prepareWith(t, preflight.Decisions{SkipContainers: []string{"kept"}}, func(w http.ResponseWriter, p, shared string) bool {
		switch p {
		case "/containers/json":
			io.WriteString(w, `[{"Id":"kept-id","Names":["/kept"],"State":"exited"}]`)
		case "/containers/kept-id/json":
			io.WriteString(w, `{"Id":"kept-id","Name":"/kept","Mounts":[{"Type":"bind","Source":"`+shared+`"}]}`)
		default:
			return false
		}
		return true
	})
	if !strings.Contains(failed, "保留的已有容器 kept") || len(stopped) > 0 {
		t.Fatalf("应在改动之前停下：%q，停过 %v", failed, stopped)
	}
}

// 正在运行的已有容器读不到配置：没法确认它有没有在用要写入的数据，不能继续；
// Docker 明确说容器不存在（刚好被删掉）时照常继续。
func TestPrepareUnreadableContainer(t *testing.T) {
	for _, code := range []int{500, 404} {
		failed, _ := prepareWith(t, preflight.Decisions{}, func(w http.ResponseWriter, p, _ string) bool {
			switch p {
			case "/containers/json":
				io.WriteString(w, `[{"Id":"other-id","Names":["/other"],"State":"running"}]`)
			case "/containers/other-id/json":
				http.Error(w, `{"message":"读不到"}`, code)
			default:
				return false
			}
			return true
		})
		if strings.Contains(failed, "没法确认它有没有在用") != (code == 500) || code == 404 && failed != "" {
			t.Errorf("Docker 回答 %d 时：%q", code, failed)
		}
	}
}

// 回滚时要原样重建原有的 Compose 容器：读它们的配置时也要带上用户指定的 MAC 地址（Docker 29 起读出来
// 的配置里没有了，见 docker.InspectForRebuild），不然回滚后网卡用的是随机的 MAC。
func TestBackupProjectKeepsFixedMAC(t *testing.T) {
	dc, _ := faultyDockerAt(t, "1.56", func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/v1.51/containers/abcdef1234567890/json":
			w.Write([]byte(`{"Id":"abcdef1234567890","Config":{"Image":"alpine","MacAddress":"02:42:ac:14:00:0a"}}`))
		case strings.HasSuffix(r.URL.Path, "/changes"):
			return false
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			w.Write([]byte(`{"Id":"abcdef1234567890","Name":"/blog-app-1","State":{"Status":"running","Running":true},"Config":{"Image":"alpine"}}`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		default:
			return false
		}
		return true
	})
	j, err := openJournal(filepath.Join(t.TempDir(), "rb"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(&out, false, nil)}, j: j}
	r.backupProject(context.Background(), "blog", []docker.ContainerSummary{{ID: "abcdef1234567890", Names: []string{"/blog-app-1"}}})
	j.close()
	entries, err := readJournal(j.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Op == opComposeOld && (len(e.Old) != 1 || !strings.Contains(string(e.Old[0].Raw), `"MacAddress":"02:42:ac:14:00:0a"`)) {
			t.Fatalf("原有容器的配置里应带上指定的 MAC：%s", e.Old[0].Raw)
		}
	}
}
