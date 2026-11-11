package dockertest

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker"
)

// 先按登记的回答，再交给 handle，都没有就回答 500；交出去的路径和记下的请求都不带接口版本的前缀。
func TestServer(t *testing.T) {
	for _, opts := range [][]Option{nil, {API("1.53"), Socket()}} {
		s := New(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/containers/missing/json" {
				if Version(r) == "" {
					t.Errorf("应记下去掉的接口版本")
				}
				return NotFound(w, r)
			}
			return false
		}, opts...)
		s.Reply("GET /containers/web/json", `{"Id":"abc","Name":"/web"}`)
		dc := s.Client()
		ctx := context.Background()
		if c, _, err := dc.InspectContainer(ctx, "web"); err != nil || c.ID != "abc" {
			t.Fatalf("登记的回答：%+v %v", c, err)
		}
		if _, _, err := dc.InspectContainer(ctx, "missing"); !docker.IsNotFound(err) {
			t.Errorf("handle 回答的 404：%v", err)
		}
		if err := dc.StartContainer(ctx, "web"); err == nil || docker.IsNotFound(err) {
			t.Errorf("没人回答的应是 500：%v", err)
		}
		want := []string{"GET /containers/web/json", "GET /containers/missing/json", "POST /containers/web/start"}
		if got := s.Calls(); !slices.Equal(got, want) {
			t.Errorf("记下的请求：%v", got)
		}
	}
}
