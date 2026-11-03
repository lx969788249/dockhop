package testpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFail(t *testing.T) {
	get := func(k string) string { return map[string]string{"DOCKHOP_TEST_FAIL": "volume:db, commit-cleanup"}[k] }
	if !Fail(get, "volume:db") || !Fail(get, "commit-cleanup") || Fail(get, "volume") || Fail(nil, "volume:db") {
		t.Error("只在列出的地方模拟失败")
	}
}

// 在指定的地方停住，测试删掉标记文件后继续；别的地方不停。
func TestPause(t *testing.T) {
	dir := t.TempDir()
	get := func(k string) string { return map[string]string{"DOCKHOP_TEST_PAUSE": "after-stop"}[k] }
	Pause(context.Background(), get, dir, "after-data") // 不是这里：马上返回
	done := make(chan struct{})
	go func() { Pause(context.Background(), get, dir, "after-stop"); close(done) }()
	marker := filepath.Join(dir, "test-paused-after-stop")
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("应在标记文件删掉之前停住")
	default:
	}
	os.Remove(marker)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("删掉标记文件后应继续")
	}
}
