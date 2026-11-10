package backup

import (
	"archive/tar"
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

// 项目目录、项目外的文件本身是软链接时（例如 /root/app 指向数据盘上的 /data/app）：打包、统计的是它
// 指向的内容，名字还是原来的路径。以前项目目录什么都没打包（WalkDir 不进入软链接），项目外的文件只
// 打包了一个指向旧服务器路径的软链接，迁过去起不来。已经作为挂载目录迁移的子目录按真实路径排除。
func TestHostTarFollowsLinkedRoots(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "data", "app")
	os.MkdirAll(filepath.Join(real, "conf"), 0o755)
	os.MkdirAll(filepath.Join(real, "db"), 0o755)
	os.WriteFile(filepath.Join(real, "compose.yml"), []byte("services: {}\n"), 0o644)
	os.WriteFile(filepath.Join(real, "conf", "app.ini"), []byte("x=1\n"), 0o644)
	os.WriteFile(filepath.Join(real, "db", "big"), []byte("mounted separately"), 0o644)
	project := filepath.Join(root, "app")
	os.Symlink(real, project)
	secret := filepath.Join(root, "secrets", "app.env")
	os.MkdirAll(filepath.Dir(secret), 0o755)
	os.WriteFile(secret, []byte("A=1\n"), 0o600)
	env := filepath.Join(root, "etc", "app.env")
	os.MkdirAll(filepath.Dir(env), 0o755)
	os.Symlink(secret, env)
	db, _ := filepath.EvalSymlinks(filepath.Join(real, "db")) // 挂载目录记的是真实路径

	names := func(h hostTar) string {
		var buf bytes.Buffer
		if err := h.write(&buf); err != nil {
			t.Fatal(err)
		}
		var out []string
		tr := tar.NewReader(&buf)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, hdr.Name+" "+string(hdr.Typeflag))
		}
		return strings.Join(out, ",")
	}
	if got := names(hostTar{ctx: context.Background(), roots: []string{project}, base: project, excludes: []string{db}, logf: t.Logf}); got != "compose.yml 0,conf/ 5,conf/app.ini 0" {
		t.Errorf("项目目录打包了：%s", got)
	}
	if got, want := names(hostTar{ctx: context.Background(), roots: []string{env}, base: "/", logf: t.Logf}), strings.TrimPrefix(env, "/")+" 0"; got != want {
		t.Errorf("项目外的文件打包了：%s，应为 %s", got, want)
	}
	if size, _ := treeSize(context.Background(), project, []string{db}, 0); size != int64(len("services: {}\n")+len("x=1\n")) {
		t.Errorf("项目目录的大小：%d", size)
	}
}

// 项目目录本身是软链接时，打包的是它指向的目录：指向系统目录或者它自己的上级目录时不能这样打包
// （会把整个系统目录、或者别的项目和 DockHop 自己的文件一起打进去），要停下说明。
func TestProjectDirLinkGuard(t *testing.T) {
	root := t.TempDir()
	apps := filepath.Join(root, "apps")
	os.MkdirAll(filepath.Join(root, "disk", "app"), 0o755)
	os.MkdirAll(apps, 0o755)
	for name, target := range map[string]string{"ok": filepath.Join(root, "disk", "app"), "parent": apps, "system": "/usr"} {
		link := filepath.Join(apps, name)
		os.Symlink(target, link)
		_, err := projectDir(link)
		if (name == "ok") != (err == nil) {
			t.Errorf("%s：%v", name, err)
		}
	}
	if real, err := projectDir(filepath.Join(root, "disk", "app")); err != nil || !strings.HasSuffix(real, filepath.Join("disk", "app")) {
		t.Errorf("普通目录：%s %v", real, err)
	}
	// 不经过软链接的项目目录照原样打包，哪怕它是系统目录（例如 compose.yml 直接放在 /root 下）。
	if real, err := projectDir("/usr"); err != nil || real != "/usr" {
		t.Errorf("普通的系统目录：%s %v", real, err)
	}
}

// DockHop 自己的目录在项目目录（软链接）指向的目录里时，也要排除：按真实路径比较。
func TestOwnDirsThroughLink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "disk", "app")
	home := filepath.Join(real, ".dockhop")
	os.MkdirAll(filepath.Join(home, "work", "out-1"), 0o755)
	os.WriteFile(filepath.Join(real, "compose.yml"), []byte("services: {}\n"), 0o644)
	link := filepath.Join(root, "app")
	os.Symlink(real, link)
	r := &run{Deps: Deps{Home: home}, workdir: filepath.Join(home, "work", "out-1")}
	var buf bytes.Buffer
	if err := (hostTar{ctx: context.Background(), roots: []string{link}, base: link, excludes: r.ownDirs(link), logf: t.Logf}).write(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), ".dockhop") {
		t.Errorf("DockHop 自己的目录不应打进迁移包：%v", r.ownDirs(link))
	}
}
