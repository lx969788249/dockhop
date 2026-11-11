package helper

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker/dockertest"
	"github.com/lx969788249/dockhop/internal/host"
)

// 只删自己的、以及创建它的任务已经结束的临时容器；别的任务还在运行时（拿着它的锁）不删。
// 老版本的（没记是谁的）只在同一个目录里没有别的任务在运行时才删。
func TestClean(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "locks", "in.lock"), filepath.Join(dir, "locks", "out.lock")
	other := filepath.Join(t.TempDir(), "locks", "out.lock") // 另一个 DOCKHOP_HOME
	helpers := map[string]string{"mine": in, "running": other, "ended": filepath.Join(dir, "gone", "out.lock"), "legacy": "1", "user": ""}
	for _, outRunning := range []bool{false, true} {
		running, err := host.TryLock(other)
		if err != nil {
			t.Fatal(err)
		}
		var outLock *host.Lock
		if outRunning {
			if outLock, err = host.TryLock(out); err != nil {
				t.Fatal(err)
			}
		}
		var mu sync.Mutex
		var removed []string
		fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
			switch {
			case strings.HasSuffix(r.URL.Path, "/containers/json"):
				var list []string
				for id, lock := range helpers {
					labels := `{}`
					if id != "user" {
						labels = fmt.Sprintf(`{%q:%q}`, Label, lock)
					}
					list = append(list, fmt.Sprintf(`{"Id":%q,"Names":["/dockhop-helper-%s"],"Labels":%s}`, id, id, labels))
				}
				fmt.Fprint(w, "["+strings.Join(list, ",")+"]")
			case r.Method == http.MethodDelete:
				mu.Lock()
				removed = append(removed, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
			return true
		})
		dc := fake.Client()
		Clean(context.Background(), dc, in)
		running.Unlock()
		outLock.Unlock()
		slices.Sort(removed)
		want := "ended,legacy,mine"
		if outRunning {
			want = "ended,mine"
		}
		if strings.Join(removed, ",") != want {
			t.Errorf("迁出在运行=%v：删了 %v，应删 %s", outRunning, removed, want)
		}
	}
}

// 标签上的锁是相对路径时（老版本的 DOCKHOP_HOME 写的相对路径），从这里认不出是哪个文件，
// 也就判断不了那个任务还在不在：不删。
func TestCleanKeepsUnknownOwner(t *testing.T) {
	var removed []string
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			fmt.Fprint(w, `[{"Id":"rel","Names":["/dockhop-helper-x-1"],"Labels":{"dockhop.helper":"home/locks/out.lock"}}]`)
		case r.Method == http.MethodDelete:
			removed = append(removed, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
		return true
	})
	dc := fake.Client()
	Clean(context.Background(), dc, filepath.Join(t.TempDir(), "locks", "in.lock"))
	if len(removed) > 0 {
		t.Errorf("认不出是谁的临时容器不应删：%v", removed)
	}
}
