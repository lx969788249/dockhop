package bundle

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type member struct {
	name, link, body string
	typ              byte
	mode             int64
}

func archive(t *testing.T, ms ...member) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range ms {
		h := &tar.Header{Name: m.name, Typeflag: m.typ, Linkname: m.link, Mode: m.mode, Size: int64(len(m.body)), ModTime: time.Unix(1700000000, 0)}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if m.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if m.typ == tar.TypeReg {
			tw.Write([]byte(m.body))
		}
	}
	tw.Close()
	return bytes.NewReader(buf.Bytes())
}

func TestScanArchive(t *testing.T) {
	ok := []member{
		{name: "srv/data/", typ: tar.TypeDir, mode: 0o755},
		{name: "srv/data/a.txt", typ: tar.TypeReg, body: "a"},
		{name: "srv/data/outside", typ: tar.TypeSymlink, link: "/etc/passwd"}, // 指向外面的软链接原样保留
		{name: "srv/data/loop", typ: tar.TypeSymlink, link: "loop"},
		{name: "srv/data/hard", typ: tar.TypeLink, link: "srv/data/a.txt"},
	}
	if err := ScanArchive(archive(t, ok...), "srv/data"); err != nil {
		t.Fatalf("正常的归档被拒绝：%v", err)
	}
	bad := map[string][]member{
		"通过软链接写到外面": {
			{name: "srv/data/", typ: tar.TypeDir},
			{name: "srv/data/etc", typ: tar.TypeSymlink, link: "/etc"},
			{name: "srv/data/etc/passwd", typ: tar.TypeReg, body: "x"},
		},
		"不在目录下面":  {{name: "srv/data/", typ: tar.TypeDir}, {name: "srv/other/x", typ: tar.TypeReg}},
		"路径里有 ..": {{name: "srv/data/", typ: tar.TypeDir}, {name: "srv/data/../../etc/x", typ: tar.TypeReg}},
		"绝对路径":    {{name: "/etc/x", typ: tar.TypeReg}},
		"硬链接指向外面": {{name: "srv/data/", typ: tar.TypeDir}, {name: "srv/data/h", typ: tar.TypeLink, link: "etc/shadow"}},
		"重复的文件": {
			{name: "srv/data/", typ: tar.TypeDir},
			{name: "srv/data/a", typ: tar.TypeReg}, {name: "srv/data/a", typ: tar.TypeSymlink, link: "/x"},
		},
		"缺少根目录": {{name: "srv/data/a", typ: tar.TypeReg}},
	}
	for what, ms := range bad {
		if err := ScanArchive(archive(t, ms...), "srv/data"); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%s：应拒绝，实际 %v", what, err)
		}
	}
	// 没有限制目录时（Compose 项目目录），相对路径都可以。
	if err := ScanArchive(archive(t, member{name: "compose.yml", typ: tar.TypeReg}, member{name: "web/index.html", typ: tar.TypeReg})); err != nil {
		t.Errorf("项目目录：%v", err)
	}
	// 多个根（项目目录外的文件）。
	if err := ScanArchive(archive(t, member{name: "etc/app.env", typ: tar.TypeReg}, member{name: "opt/ctx/", typ: tar.TypeDir}, member{name: "opt/ctx/Dockerfile", typ: tar.TypeReg}),
		"/etc/app.env", "/opt/ctx"); err != nil {
		t.Errorf("多个根：%v", err)
	}
}

func TestExtractTree(t *testing.T) {
	dst := t.TempDir()
	err := ExtractTree(archive(t,
		member{name: "data/", typ: tar.TypeDir, mode: 0o750},
		member{name: "data/a.txt", typ: tar.TypeReg, body: "hello", mode: 0o640},
		member{name: "data/link", typ: tar.TypeSymlink, link: "/nonexistent/target"},
		member{name: "data/hard", typ: tar.TypeLink, link: "data/a.txt"},
		member{name: "data/fifo", typ: tar.TypeFifo, mode: 0o600},
	), dst)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dst, "data", "a.txt"))
	st, _ := os.Stat(filepath.Join(dst, "data", "a.txt"))
	if string(b) != "hello" || st.Mode().Perm() != 0o640 || !st.ModTime().Equal(time.Unix(1700000000, 0)) {
		t.Errorf("文件内容、权限或修改时间不对：%q %v %v", b, st.Mode(), st.ModTime())
	}
	if l, _ := os.Readlink(filepath.Join(dst, "data", "link")); l != "/nonexistent/target" {
		t.Errorf("悬空的软链接应原样保留：%q", l)
	}
	if hs, _ := os.Stat(filepath.Join(dst, "data", "hard")); !os.SameFile(hs, st) {
		t.Error("硬链接应指向同一个文件")
	}
	if fs, err := os.Lstat(filepath.Join(dst, "data", "fifo")); err != nil || fs.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("FIFO 应保留：%v", err)
	}
	if ds, _ := os.Stat(filepath.Join(dst, "data")); ds.Mode().Perm() != 0o750 {
		t.Errorf("目录权限：%v", ds.Mode())
	}
}
