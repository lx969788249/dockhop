package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeDocker 在 unix socket 上模拟一个 Docker，记录收到的请求路径。
type fakeDocker struct {
	apiVersion string
	mu         sync.Mutex
	paths      []string
	lastBody   string
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		f.lastBody = string(b)
	}
	f.mu.Unlock()
	v := "/v" + f.apiVersion
	if f.apiVersion > MaxAPIVersion {
		v = "/v" + MaxAPIVersion
	}
	switch r.URL.Path {
	case "/version":
		fmt.Fprintf(w, `{"Version":"test","ApiVersion":%q,"MinAPIVersion":"1.24","Os":"linux","Arch":"amd64"}`, f.apiVersion)
	case v + "/info":
		fmt.Fprint(w, `{"ServerVersion":"test","Architecture":"x86_64","NCPU":2,"Runtimes":{"runc":{}},"NewFieldFromFuture":true}`)
	case v + "/containers/json":
		fmt.Fprint(w, `[{"Id":"0123456789abcdef","Names":["/web"],"State":"running","Labels":{"a":"b"}}]`)
	case v + "/containers/web/json":
		fmt.Fprint(w, `{"Id":"0123456789abcdef","Config":{"Image":"nginx","FutureField":{"x":1}}}`)
	case v + "/containers/fixedmac/json":
		fmt.Fprint(w, `{"Id":"f","Config":{"Image":"nginx","FutureField":{"x":1}},"NetworkSettings":{"Networks":{"appnet":{"MacAddress":"02:42:ac:14:00:0a"}}}}`)
	case "/v1.51/containers/fixedmac/json":
		fmt.Fprint(w, `{"Id":"f","Config":{"Image":"nginx","MacAddress":"02:42:ac:14:00:0a"}}`)
	case "/v1.51/containers/web/json":
		fmt.Fprint(w, `{"Id":"0123456789abcdef","Config":{"Image":"nginx"}}`)
	case v + "/containers/missing/json":
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"No such container: missing"}`)
	case v + "/containers/create":
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"Id":"new"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"message":"unexpected %s"}`, r.URL.Path)
	}
}

func startFake(t *testing.T, apiVersion string) (*fakeDocker, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{apiVersion: apiVersion}
	srv := &http.Server{Handler: f}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f, "unix://" + sock
}

func TestNegotiateAndRequests(t *testing.T) {
	f, host := startFake(t, "1.41")
	ctx := context.Background()
	c, err := New(ctx, WithHost(host))
	if err != nil {
		t.Fatal(err)
	}
	if c.APIVersion() != "1.41" || c.AtLeast("1.44") || !c.Local() {
		t.Fatalf("协商结果不对：%s %v %v", c.APIVersion(), c.AtLeast("1.44"), c.Local())
	}
	info, err := c.Info(ctx)
	if err != nil || info.Architecture != "x86_64" || info.NCPU != 2 {
		t.Fatalf("Info：%+v %v", info, err)
	}
	list, err := c.Containers(ctx, true)
	if err != nil || len(list) != 1 || list[0].Name() != "web" {
		t.Fatalf("Containers：%+v %v", list, err)
	}
	// 原始 JSON 必须保留客户端不认识的字段，才能原样重建容器。
	raw, err := c.InspectContainerRaw(ctx, "web")
	if err != nil || !strings.Contains(string(raw), `"FutureField":{"x":1}`) {
		t.Fatalf("原始配置丢了字段：%s %v", raw, err)
	}
	if _, err := c.InspectContainerRaw(ctx, "missing"); !IsNotFound(err) || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("不存在时应返回 404 错误：%v", err)
	}
	var created struct{ Id string }
	if err := c.Post(ctx, "/containers/create", nil, raw, &created); err != nil || created.Id != "new" {
		t.Fatalf("Post：%+v %v", created, err)
	}
	if f.lastBody != string(raw) {
		t.Fatalf("原始 JSON 应原样发送：%s", f.lastBody)
	}
	if !strings.HasPrefix(f.paths[1], "GET /v1.41/") {
		t.Fatalf("请求应带协商后的版本前缀：%v", f.paths)
	}
}

func TestNewerDockerUsesTestedVersion(t *testing.T) {
	_, host := startFake(t, "1.60")
	c, err := New(context.Background(), WithHost(host))
	if err != nil {
		t.Fatal(err)
	}
	if c.APIVersion() != MaxAPIVersion || !c.AtLeast("1.44") {
		t.Fatalf("比测试过的更新时应使用 %s，实际 %s", MaxAPIVersion, c.APIVersion())
	}
}

func TestOldDockerRejected(t *testing.T) {
	_, host := startFake(t, "1.40")
	_, err := New(context.Background(), WithHost(host))
	if err == nil || !strings.Contains(err.Error(), "需要 Docker 20.10 或更新的版本") {
		t.Fatalf("太旧的 Docker 应拒绝：%v", err)
	}
}

func TestNotRunning(t *testing.T) {
	_, err := New(context.Background(), WithHost("unix:///nonexistent/docker.sock"))
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("连不上时应返回 ErrNotRunning：%v", err)
	}
	if _, err := New(context.Background(), WithHost("ssh://root@host")); err == nil {
		t.Fatal("不支持的 DOCKER_HOST 应报错")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{{"1.41", "1.44", -1}, {"1.53", "1.53", 0}, {"1.100", "1.53", 1}, {"2", "1.99", 1}}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%s, %s) = %d", c.a, c.b, got)
		}
	}
}

// 接口 1.52 起，容器配置里不再写出用户指定的 MAC 地址（网络设置里的 MAC 自动分配的也有，看不出来）：
// 迁出时按 1.51 的接口再问一次，补进原始配置，其他内容原样不动。老版本的 Docker 不用多问。
func TestInspectForRebuildAddsFixedMAC(t *testing.T) {
	ctx := context.Background()
	_, host := startFake(t, "1.56")
	c, err := New(ctx, WithHost(host))
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := c.InspectForRebuild(ctx, "fixedmac")
	var top struct{ Config map[string]any }
	if err != nil || json.Unmarshal(raw, &top) != nil || top.Config["MacAddress"] != "02:42:ac:14:00:0a" ||
		!strings.Contains(string(raw), `"FutureField":{"x":1}`) {
		t.Fatalf("应在 Config 里补上用户指定的 MAC，其他原样不动：%s %v", raw, err)
	}
	if _, raw, err = c.InspectForRebuild(ctx, "web"); err != nil || strings.Contains(string(raw), "MacAddress") {
		t.Fatalf("没指定 MAC 的不补：%s %v", raw, err)
	}
	f, host := startFake(t, "1.43")
	if c, err = New(ctx, WithHost(host)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.InspectForRebuild(ctx, "web"); err != nil || strings.Contains(strings.Join(f.paths, " "), "/v1.51/") {
		t.Fatalf("老版本的 Docker 配置里本来就有，不用多问：%v %v", f.paths, err)
	}
}
