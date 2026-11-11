package backup

import (
	"io"
	"testing"

	"github.com/lx969788249/dockhop/internal/ui"
)

// testRun 是测试用的 run，和迁出时一样用 newRun 造；没给的依赖补上测试用的：屏幕丢掉输出、环境变量都是空的、
// DockHop 的目录放在临时目录里。迁出走到哪一步才有的内容由测试自己填。
func testRun(t *testing.T, d Deps, o Options) *run {
	t.Helper()
	if d.Screen == nil {
		d.Screen = ui.New(io.Discard, false, nil)
	}
	if d.Getenv == nil {
		d.Getenv = func(string) string { return "" }
	}
	if d.Home == "" {
		d.Home = t.TempDir()
	}
	return newRun(d, o)
}
