package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 只有正式发布的版本、设置了地址、用户没有关闭时才访问计数。
func TestCountOn(t *testing.T) {
	cases := []struct {
		version, count string
		env            map[string]string
		on             bool
	}{
		{"1.1.2", "http://x", nil, true},
		{"1.1.2", "", nil, false},
		{"dev", "http://x", nil, false},
		{"0.0.0-ci", "http://x", nil, false},
		{"1.1.2", "http://x", map[string]string{"DOCKHOP_NO_STATS": "1"}, false},
		{"1.1.2", "http://x", map[string]string{"DO_NOT_TRACK": "1"}, false},
		{"1.1.2", "http://x", map[string]string{"DO_NOT_TRACK": "0"}, true},
	}
	for _, c := range cases {
		e := Env{Version: c.version, Count: c.count, Getenv: func(k string) string { return c.env[k] }}
		if got := e.countOn(); got != c.on {
			t.Errorf("%s %q %v：%v，应为 %v", c.version, c.count, c.env, got, c.on)
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
	menu := func(url string) (string, time.Duration) {
		var errb bytes.Buffer
		start := time.Now()
		Run(nil, Env{Stderr: &errb, Stdout: &bytes.Buffer{}, TTY: true, Input: strings.NewReader("1\n"), Version: "1.1.2", Count: url,
			Getenv: func(string) string { return "" }, HomeDir: func() (string, error) { return t.TempDir(), nil }})
		return errb.String(), time.Since(start)
	}
	if out, _ := menu(srv.URL + "?case=ok"); !strings.Contains(out, "已成功完成迁移：") || !strings.Contains(out, "1234") {
		t.Errorf("应显示次数：%q", out)
	}
	for _, c := range []string{"quota", "html", "slow", "closed"} {
		url := srv.URL + "?case=" + c
		if c == "closed" {
			url = "http://127.0.0.1:1/count"
		}
		out, took := menu(url)
		if strings.Contains(out, "已成功完成迁移") || !strings.Contains(out, "1) 迁出") || took > 2500*time.Millisecond {
			t.Errorf("%s：不应显示次数，菜单照常出来（用时 %s）：%q", c, took, out)
		}
	}

	// 迁入成功后加一：带着版本号发一个空的 POST。
	countMigration(&Env{Version: "1.1.2", Count: srv.URL + "?case=ok", Context: t.Context(), Getenv: func(string) string { return "" }})
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 1 || posts[0] != "DockHop/1.1.2" {
		t.Errorf("应发一次计数：%v", posts)
	}
}
