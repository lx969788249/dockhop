package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// slowWriter 每写一段停一下，模拟往压缩、加密的迁移包里写一个很大的文件。
type slowWriter struct{}

func (slowWriter) Write(p []byte) (int, error) {
	time.Sleep(time.Millisecond)
	return len(p), nil
}

// 停机打包一个很大的文件时按 Ctrl+C：马上停下（停着的容器才能尽快恢复），不用等文件读完。
func TestCopyFileStopsOnCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.img")
	f, _ := os.Create(path)
	f.Truncate(256 << 20) // 稀疏文件，不占磁盘
	f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	err := copyFile(ctx, slowWriter{}, path, 256<<20, t.Logf)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("应马上停下：%v（用了 %s）", err, time.Since(start))
	}
	// 没取消时照常完整复制。
	small := filepath.Join(t.TempDir(), "small")
	os.WriteFile(small, []byte("hello"), 0o600)
	if err := copyFile(context.Background(), io.Discard, small, 5, t.Logf); err != nil {
		t.Fatal(err)
	}
}
