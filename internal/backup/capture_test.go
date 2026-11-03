package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
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

// 旧服务器打包的 Compose 项目外部文件，新服务器要能通过检查：build 上下文目录和它里面
// 被 env_file 写到的文件同时出现时，同一个文件不能打包两次。
func TestOutsideArchiveAccepted(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"app", "shared", "config"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "shared", "app.env"), []byte("MODE=prod\n"), 0o644)
	os.WriteFile(filepath.Join(root, "shared", "Dockerfile"), []byte("FROM alpine\n"), 0o644)
	prod := filepath.Join(root, "config", "prod.yml")
	os.WriteFile(prod, []byte("services:\n  web: {environment: {MODE: prod}}\n"), 0o644)
	main := filepath.Join(root, "app", "compose.yml")
	os.WriteFile(main, []byte("services:\n  web: {build: ../shared, env_file: ../shared/app.env}\n"), 0o644)

	roots, _ := compose.DeclaredFiles(compose.Project{Name: "app", WorkingDir: filepath.Join(root, "app"), Files: []string{main, prod}})
	if want := []string{prod, filepath.Join(root, "shared")}; !slices.Equal(roots, want) {
		t.Fatalf("要一起迁移的文件：%v，应为 %v", roots, want)
	}
	var archive bytes.Buffer
	if err := (hostTar{ctx: context.Background(), roots: roots, base: "/", logf: t.Logf}).write(&archive); err != nil {
		t.Fatal(err)
	}
	trimmed := make([]string, len(roots))
	for i, r := range roots {
		trimmed[i] = strings.TrimPrefix(r, "/")
	}
	if err := bundle.ScanArchive(bytes.NewReader(archive.Bytes()), trimmed...); err != nil {
		t.Fatalf("新服务器不接受打包的文件：%v", err)
	}
}
