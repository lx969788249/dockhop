package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 任务结束时：成功、还没改动就取消了，日志删掉，空了的目录和 DockHop 的目录也删掉，服务器上
// 不留东西；失败时日志留着。
func TestFinishLeavesNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		keep bool
	}{{"成功", nil, false}, {"取消", ui.ErrCancelled, false}, {"失败", errReported, true}} {
		home := filepath.Join(t.TempDir(), ".dockhop")
		for _, d := range []string{"work", "state", "rollback", "bin", "locks"} {
			os.MkdirAll(filepath.Join(home, d), 0o700)
		}
		log, err := logging.Open(filepath.Join(home, "logs"), "out", nil)
		if err != nil {
			t.Fatal(err)
		}
		tk := &task{env: &Env{Getenv: func(string) string { return "" }}, home: home, log: log}
		tk.finish(c.err)
		if _, err := os.Stat(log.Path()); (err == nil) != c.keep {
			t.Errorf("%s：日志留着=%v，应为 %v", c.name, err == nil, c.keep)
		}
		left, _ := os.ReadDir(home)
		if c.keep && (len(left) != 1 || left[0].Name() != "logs") {
			t.Errorf("%s：应只剩日志：%v", c.name, left)
		}
		if _, err := os.Stat(home); !c.keep && !os.IsNotExist(err) {
			t.Errorf("%s：DockHop 的目录应删掉：%v", c.name, left)
		}
	}
}

// 只删 DockHop 自己建的空目录：有内容的、不认识的都不动，这时 DockHop 的目录也留着。
func TestTidyHomeKeepsContent(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".dockhop")
	os.MkdirAll(filepath.Join(home, "work", "in-x"), 0o700)
	os.MkdirAll(filepath.Join(home, "mine"), 0o700)
	os.MkdirAll(filepath.Join(home, "locks"), 0o700)
	tidyHome(home)
	for p, keep := range map[string]bool{"work/in-x": true, "mine": true, "locks": false} {
		if _, err := os.Stat(filepath.Join(home, p)); (err == nil) != keep {
			t.Errorf("%s：留着=%v，应为 %v", p, err == nil, keep)
		}
	}
}
