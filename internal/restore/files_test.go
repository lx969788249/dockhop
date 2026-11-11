package restore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// 跨磁盘移动时复制到一半失败：不能留下一个看起来完整的备份，原件也不能动。
// 否则回滚只看备份在不在，会拿半份备份覆盖完整的原件。
func TestMoveOrCopyPartialFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 能读没有权限的文件，模拟不了复制失败")
	}
	rename = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EXDEV} }
	defer func() { rename = os.Rename }()
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), bytes.Repeat([]byte("a"), 16<<10), 0o644)
	os.WriteFile(filepath.Join(src, "b.txt"), []byte("b"), 0o000) // 读不了：复制到这里失败
	if err := moveOrCopy(src, dst); err == nil {
		t.Fatal("应失败")
	}
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("失败时不能留下备份：%v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "*partial*")); len(left) > 0 {
		t.Errorf("临时文件没删：%v", left)
	}
	if b, _ := os.ReadFile(filepath.Join(src, "a.txt")); len(b) != 16<<10 {
		t.Fatal("原件不能动")
	}
	// 能读的时候：完整复制过去，删掉原来的。
	os.Chmod(filepath.Join(src, "b.txt"), 0o644)
	if err := moveOrCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); len(b) != 16<<10 {
		t.Fatal("备份不完整")
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("原来的应删除")
	}
}

// 修改时间设不上不算失败（有的网络盘、FUSE 不让改）：它不影响数据，也不影响程序能不能用。这里让设修改
// 时间这一步失败：不是 root 时不改属主，软链接不改权限，只剩它会去碰这个不存在的路径。
func TestApplyMetaModTimeBestEffort(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 会先改属主")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := applyMeta(missing, FileMeta{Link: true, MTime: time.Now().UnixNano()}); err != nil {
		t.Fatalf("修改时间设不上不应算失败：%v", err)
	}
}
