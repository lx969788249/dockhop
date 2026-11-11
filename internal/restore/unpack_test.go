package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 检查 Compose 配置时，报错说的是临时目录里没有的文件（迁入时会放回去），不算数；说的是
// 临时目录里有的文件（读不懂它），算数。
func TestMissingIn(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("x"), 0o600)
	if !missingIn("env file "+dir+"/conf/app.env not found: stat "+dir+"/conf/app.env: no such file or directory", dir) {
		t.Error("没有的文件应算作缺文件")
	}
	if missingIn(dir+"/compose.yml: yaml: line 3: mapping values are not allowed", dir) {
		t.Error("有的文件读不懂，不是缺文件")
	}
}

// 解开后发现迁移包里的内容有问题：下载的文件重新运行也用不上，删掉。只有解密发现被改坏、而迁移
// 计划没有记 SHA-256（旧版本）时，才可能是下载时出的错，提示重新下载；其余只能在旧服务器重新迁出。
func TestBadBundleDropsDownload(t *testing.T) {
	cases := []struct {
		name              string
		corrupt, verified bool
		redownload        bool
	}{
		{"内容不合规、核对过", false, true, false},
		{"内容不合规、计划没记校验值", false, false, false}, // 解密通过就是旧服务器打包的内容
		{"被改坏、核对过", true, true, false},         // 旧服务器上的文件本身就坏了
		{"被改坏、计划没记校验值", true, false, true},
	}
	for _, c := range cases {
		dir := t.TempDir()
		link, _ := crypto.NewLink("127.0.0.1", 1)
		path := filepath.Join(dir, "bundle")
		f, _ := os.Create(path)
		w, _ := bundle.NewWriter(f, link)
		var tb bytes.Buffer
		tw := tar.NewWriter(&tb)
		tw.WriteHeader(&tar.Header{Name: "dockhop-data/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: "dockhop-data/etc", Typeflag: tar.TypeSymlink, Linkname: "/etc"})
		tw.WriteHeader(&tar.Header{Name: "dockhop-data/etc/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
		tw.Write([]byte("evil"))
		tw.Close()
		parts, n, _ := w.AddStream("volumes/v", &tb)
		w.AddJSON("manifest.json", bundle.Manifest{Format: bundle.Format, Version: bundle.Version,
			Volumes: []bundle.Volume{{Name: "v", Driver: "local", Bytes: n, Parts: parts}}})
		w.Close()
		f.Close()
		if c.corrupt {
			b, _ := os.ReadFile(path)
			b[len(b)-20] ^= 0xff
			os.WriteFile(path, b, 0o600)
		}
		plan := &bundle.Plan{}
		if c.verified {
			plan.BundleSHA256, _ = fileSHA256(path)
		}
		var out bytes.Buffer
		r := testRun(t, Deps{Screen: ui.New(&out, false, nil), Home: dir}, Options{Link: link})
		r.plan = plan
		r.downloaded = true
		r.bundlePath = path
		r.dataDir = filepath.Join(dir, "data")
		_, err := r.unpack(context.Background())
		got := r.Screen.Failure()
		if err == nil || got == nil || !strings.Contains(got.Hint, "在旧服务器重新运行迁出") || strings.Contains(got.Hint, "重新下载") != c.redownload {
			t.Errorf("%s：%v %+v", c.name, err, got)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s：下载的迁移包应删掉", c.name)
		}
	}
}

// 解开后按迁移包里实际要写的目录再查一遍：目录或者它的软链接盖住 DockHop 自己的目录时不迁，
// 提示换 DOCKHOP_HOME（和迁入前检查同一条规则）。
func TestHomeClash(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "dockhop")
	app := filepath.Join(root, "app")
	r := testRun(t, Deps{Home: home}, Options{})
	r.m = &bundle.Manifest{Binds: []bundle.Bind{{Path: app}}}
	if is := r.homeClash(); is != nil {
		t.Fatalf("不重叠时不应拦下：%s", is.Title)
	}
	r.m.Binds[0].Links = []bundle.BindLink{{Link: filepath.Join(home, "cfg"), Target: app}}
	if is := r.homeClash(); is == nil || !strings.Contains(is.Title, filepath.Join(home, "cfg")) || !strings.Contains(is.Hint, "DOCKHOP_HOME") {
		t.Fatalf("软链接在 DockHop 的目录里时应拦下：%+v", is)
	}
}
