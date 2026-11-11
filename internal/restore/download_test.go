package restore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 下载的文件和迁移计划里记的 SHA-256 对不上（被换成了同一次迁移的另一个文件也一样）：删掉，
// 不能用。对得上、或者旧版本的计划没有记录时照常用。
func TestVerifyDownload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image-1")
	os.WriteFile(path, []byte("image-2 的内容"), 0o600)
	var out bytes.Buffer
	r := testRun(t, Deps{Screen: ui.New(&out, false, nil)}, Options{})
	want, _ := fileSHA256(path)
	if err := r.verifyDownload(path, want, "镜像文件"); err != nil {
		t.Fatal(err)
	}
	if err := r.verifyDownload(path, "", "镜像文件"); err != nil {
		t.Fatal(err)
	}
	if err := r.verifyDownload(path, strings.Repeat("0", 64), "镜像文件"); !errors.Is(err, errMismatch) {
		t.Fatalf("对不上时应失败：%v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("对不上的文件应删掉")
	}
}

// 1.1.6 之前的旧服务器没有在迁移计划里记镜像文件的 SHA-256：要从旧服务器下载（或者拉取失败时
// 可能改从旧服务器下载）的镜像被对调了也发现不了，下载之前就停下。本机已经有的不用下载，照常用。
func TestCheckSums(t *testing.T) {
	files := []bundle.ImageFile{{File: "image-1", Refs: []string{"app:1"}}, {File: "image-2", Refs: []string{"db:1"}, Pull: "db@sha256:abc"}}
	for _, c := range []struct {
		name   string
		images map[string]imageSource
		sums   bool
		ok     bool
	}{
		{"都要下载、没有校验值", map[string]imageSource{}, false, false},
		{"能拉取、没有校验值", map[string]imageSource{"image-1": {local: "sha256:1"}, "image-2": {pull: true}}, false, false},
		{"本机都有", map[string]imageSource{"image-1": {local: "sha256:1"}, "image-2": {local: "sha256:2"}}, false, true},
		{"有校验值", map[string]imageSource{}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := &bundle.Plan{Images: append([]bundle.ImageFile(nil), files...)}
			if c.sums {
				for i := range plan.Images {
					plan.Images[i].SHA256 = strings.Repeat("0", 64)
				}
			}
			var out bytes.Buffer
			r := testRun(t, Deps{Screen: ui.New(&out, false, nil)}, Options{})
			if err := r.checkSums(&checked{plan: plan, images: c.images}); (err == nil) != c.ok || !c.ok && !errors.Is(err, errNoChecksums) {
				t.Fatalf("结果：%v", err)
			}
		})
	}
}

// 换了新链接时，以前的链接留下的下载和记录用不上了，删掉；这个链接自己的（重试用）和迁出的不动。
func TestDropStale(t *testing.T) {
	home := t.TempDir()
	link, _ := crypto.NewLink("127.0.0.1", 1)
	cur := "in-" + link.Token
	for _, p := range []string{"work/in-old/bundle", "work/" + cur + "/bundle", "work/out-abc/bundle",
		"state/in-old.json", "state/" + cur + ".json", "state/out-stopped.json"} {
		full := filepath.Join(home, p)
		os.MkdirAll(filepath.Dir(full), 0o700)
		os.WriteFile(full, []byte("x"), 0o600)
	}
	r := testRun(t, Deps{Home: home}, Options{Link: link})
	r.dropStale()
	for p, keep := range map[string]bool{"work/in-old": false, "state/in-old.json": false, "work/" + cur: true,
		"state/" + cur + ".json": true, "work/out-abc": true, "state/out-stopped.json": true} {
		if _, err := os.Stat(filepath.Join(home, p)); (err == nil) != keep {
			t.Errorf("%s：留着=%v，应为 %v", p, err == nil, keep)
		}
	}
}
