package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/crypto"
)

// 只有正式发布的版本、用户没有关闭时才统计（没有设置地址时也不访问，见 TestCountFailed）。
func TestCountOn(t *testing.T) {
	cases := []struct {
		version string
		env     map[string]string
		on      bool
	}{
		{"1.1.2", nil, true},
		{"dev", nil, false},
		{"0.0.0-ci", nil, false},
		{"1.1.2", map[string]string{"DOCKHOP_NO_STATS": "1"}, false},
		{"1.1.2", map[string]string{"DO_NOT_TRACK": "1"}, false},
		{"1.1.2", map[string]string{"DO_NOT_TRACK": "0"}, true},
	}
	for _, c := range cases {
		e := Env{Version: c.version, Getenv: func(k string) string { return c.env[k] }}
		if got := e.countOn(); got != c.on {
			t.Errorf("%s %v：%v，应为 %v", c.version, c.env, got, c.on)
		}
	}
}

// 迁出、迁入失败时把失败的次数加一：在后台带着版本号往失败次数的地址发一个空的 POST，wait 等它
// 发完。关了统计、没有设置地址时不发。
func TestCountFailed(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("User-Agent"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	env := func(vars map[string]string) *Env {
		return &Env{Version: "1.1.2", Count: srv.URL + "/count", Failed: srv.URL + "/failed", Context: t.Context(),
			Getenv: func(k string) string { return vars[k] }}
	}
	countFailed(env(nil))()
	countFailed(env(map[string]string{"DOCKHOP_NO_STATS": "1"}))()
	noURL := env(nil)
	noURL.Failed = ""
	countFailed(noURL)()
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "、") != "POST /failed DockHop/1.1.2" {
		t.Errorf("应只发一次失败的计数：%v", got)
	}
}

// 迁出、迁入失败时（这里是要迁出的服务不存在、连不上旧服务器）发一次失败的计数，不发成功的；
// 关了统计时都不发。
func TestFailureCounted(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts = append(posts, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("DOCKER_HOST", fakeDockerSock(t))
	link, _ := crypto.NewLink("127.0.0.1", 1)
	cases := []struct {
		name string
		vars map[string]string
		args []string
		want string
	}{
		{"迁出失败", nil, []string{"out", "--include", "nosuch"}, "POST /failed"},
		{"迁入失败", nil, []string{"in", link.String()}, "POST /failed"},
		{"关了统计", map[string]string{"DOCKHOP_NO_STATS": "1"}, []string{"out", "--include", "nosuch"}, ""},
	}
	for _, c := range cases {
		mu.Lock()
		posts = nil
		mu.Unlock()
		var errb bytes.Buffer
		code := Run(c.args, Env{Stdout: &bytes.Buffer{}, Stderr: &errb, Version: "1.2.1", Platform: "linux/amd64",
			Getenv: func(k string) string { return c.vars[k] }, HomeDir: func() (string, error) { return t.TempDir(), nil },
			Context: context.Background(), Count: srv.URL + "/count", Failed: srv.URL + "/failed"})
		mu.Lock()
		got := strings.Join(posts, "、")
		mu.Unlock()
		if code != ExitFailed || got != c.want {
			t.Errorf("%s：退出码 %d，计数 %q，应为 %q\n%s", c.name, code, got, c.want, errb.String())
		}
	}
}

// 读得到时菜单里显示次数；出错、内容不对、太慢、连不上时不显示，也不报错。
func TestMenuShowsCount(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("case") {
		case "ok":
			if r.Method == http.MethodPost {
				mu.Lock()
				posts = append(posts, r.Header.Get("User-Agent"))
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Write([]byte("1234\n"))
		case "quota":
			http.Error(w, "error code: 1027", http.StatusTooManyRequests)
		case "html":
			w.Write([]byte("<html>"))
		case "slow":
			time.Sleep(3 * time.Second)
			w.Write([]byte("1234\n"))
		}
	}))
	defer srv.Close()
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	var vars map[string]string // 环境变量
	menu := func(url string) (string, time.Duration) {
		var errb bytes.Buffer
		start := time.Now()
		Run(nil, Env{Stderr: &errb, Stdout: &bytes.Buffer{}, TTY: true, Input: strings.NewReader("1\n"), Version: "1.1.2", Count: url,
			Getenv: func(k string) string { return vars[k] }, HomeDir: func() (string, error) { return t.TempDir(), nil }})
		return errb.String(), time.Since(start)
	}
	if out, _ := menu(srv.URL + "?case=ok"); !strings.Contains(out, "已协助完成迁移：") || !strings.Contains(out, "1234") {
		t.Errorf("应显示次数：%q", out)
	}
	for _, c := range []string{"quota", "html", "slow", "closed"} {
		url := srv.URL + "?case=" + c
		if c == "closed" {
			url = "http://127.0.0.1:1/count"
		}
		out, took := menu(url)
		if strings.Contains(out, "已协助完成迁移") || !strings.Contains(out, "[1]  迁出") || took > 2500*time.Millisecond {
			t.Errorf("%s：不应显示次数，菜单照常出来（用时 %s）：%q", c, took, out)
		}
	}

	// 通过一行命令运行时，run.sh 把 dockhop.run 填好的次数交过来：直接显示，不再去问（地址连不上
	// 也照样显示）；交来的不是数字时照常去读；关了统计时照样不显示。
	vars = map[string]string{"DOCKHOP_COUNT": "42"}
	if out, took := menu("http://127.0.0.1:1/count"); !strings.Contains(out, "已协助完成迁移：") || !strings.Contains(out, "42") || took > time.Second {
		t.Errorf("应直接显示交来的次数（用时 %s）：%q", took, out)
	}
	vars = map[string]string{"DOCKHOP_COUNT": "abc"}
	if out, _ := menu(srv.URL + "?case=ok"); !strings.Contains(out, "1234") {
		t.Errorf("交来的不是数字时应照常去读：%q", out)
	}
	vars = map[string]string{"DOCKHOP_COUNT": "42", "DOCKHOP_NO_STATS": "1"}
	if out, _ := menu("http://127.0.0.1:1/count"); strings.Contains(out, "已协助完成迁移") {
		t.Errorf("关了统计时不应显示：%q", out)
	}
	// 次数还少（不到 shownFrom）时不显示，满了显示真实的次数。
	for n, shown := range map[string]bool{"19": false, "20": true} {
		vars = map[string]string{"DOCKHOP_COUNT": n}
		if out, _ := menu("http://127.0.0.1:1/count"); strings.Contains(out, "已协助完成迁移：") != shown {
			t.Errorf("次数 %s：显示=%v，应为 %v：%q", n, !shown, shown, out)
		}
	}

	// 迁入成功时加一：在后台带着版本号发一个空的 POST，wait 等它发完。
	wait := countSucceeded(&Env{Version: "1.1.2", Count: srv.URL + "?case=ok", Context: t.Context(), Getenv: func(string) string { return "" }})
	wait()
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 1 || posts[0] != "DockHop/1.1.2" {
		t.Errorf("应发一次计数：%v", posts)
	}
}
