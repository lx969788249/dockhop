package bundle

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/lx969788249/dockhop/internal/crypto"
)

func TestWriteExtractRoundTrip(t *testing.T) {
	link, _ := crypto.NewLink("1.2.3.4", 8080)
	big := make([]byte, PartSize*2+12345) // 跨三段
	rand.Read(big)
	var sealed bytes.Buffer
	w, err := NewWriter(&sealed, link)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddJSON("containers/web.json", []byte(`{"raw":true}`)); err != nil {
		t.Fatal(err)
	}
	parts, n, err := w.AddStream("volumes/data", bytes.NewReader(big))
	if err != nil || parts != 3 || n != int64(len(big)) {
		t.Fatalf("AddStream = %d 段 %d 字节 %v", parts, n, err)
	}
	if parts, _, _ := w.AddStream("volumes/empty", bytes.NewReader(nil)); parts != 0 {
		t.Fatalf("空的数据流应写 0 段，实际 %d", parts)
	}
	m := Manifest{Format: Format, Version: Version, Volumes: []Volume{{Name: "data", Parts: 3, Bytes: n}}}
	if err := w.AddJSON("manifest.json", m); err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("manifest.json", nil); err == nil {
		t.Fatal("重复的文件名应报错")
	}
	if err := w.AddBytes("../evil", nil); err == nil {
		t.Fatal("不合规的文件名应报错")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if sealed.Len() >= len(big) {
		t.Logf("随机数据压不小（%d → %d），正常", len(big), sealed.Len())
	}

	dst := t.TempDir()
	var counted countWriter
	parsed, _ := crypto.ParseLink(link.String())
	if err := Extract(bytes.NewReader(sealed.Bytes()), parsed, dst, &counted); err != nil {
		t.Fatal(err)
	}
	if int(counted) != sealed.Len() {
		t.Errorf("进度应统计全部密文：%d / %d", counted, sealed.Len())
	}
	got, err := ReadManifest(dst)
	if err != nil || got.Volumes[0].Parts != 3 {
		t.Fatalf("ReadManifest：%+v %v", got, err)
	}
	rc := OpenStream(dst, "volumes/data", got.Volumes[0].Parts)
	back, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(back, big) {
		t.Fatalf("分段拼回的数据不一致：%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "containers", "web.json")); string(b) != `{"raw":true}` {
		t.Fatalf("原始 JSON 应原样保存：%s", b)
	}
	if _, err := io.ReadAll(OpenStream(dst, "volumes/data", 4)); err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Fatalf("缺段时应报错：%v", err)
	}

	tampered := append([]byte(nil), sealed.Bytes()...)
	tampered[len(tampered)/2] ^= 0xff
	if err := Extract(bytes.NewReader(tampered), link, t.TempDir(), nil); !errors.Is(err, crypto.ErrCorrupted) {
		t.Fatalf("被改动的迁移包应报 ErrCorrupted：%v", err)
	}
	other, _ := crypto.NewLink("1.2.3.4", 8080)
	if err := Extract(bytes.NewReader(sealed.Bytes()), other, t.TempDir(), nil); !errors.Is(err, crypto.ErrWrongKey) {
		t.Fatalf("密钥不对应报 ErrWrongKey：%v", err)
	}
}

type countWriter int64

func (c *countWriter) Write(p []byte) (int, error) { *c += countWriter(len(p)); return len(p), nil }

// 即使有人伪造了能解开的迁移包（拿到了密钥），解开时也不会写到目标目录以外。
func TestExtractRejectsUnsafeEntries(t *testing.T) {
	link, _ := crypto.NewLink("1.2.3.4", 8080)
	for _, h := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600},
		{Name: "/etc/passwd", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0o777},
		{Name: "dir", Typeflag: tar.TypeDir, Mode: 0o755},
	} {
		var sealed bytes.Buffer
		enc, _ := link.Encrypt(&sealed)
		zw, _ := zstd.NewWriter(enc)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(h)
		if h.Size > 0 {
			tw.Write([]byte("x"))
		}
		tw.Close()
		zw.Close()
		enc.Close()
		dst := t.TempDir()
		if err := Extract(&sealed, link, dst, nil); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%q 应被拒绝：%v", h.Name, err)
		}
	}
}

func TestReadManifestVersion(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"format":"dockhop-bundle","version":99}`), 0o600)
	if _, err := ReadManifest(dir); err == nil || !strings.Contains(err.Error(), "更新版本的 DockHop") {
		t.Fatalf("新格式应提示更新：%v", err)
	}
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"format":"other"}`), 0o600)
	if _, err := ReadManifest(dir); err == nil {
		t.Fatal("不是 DockHop 的迁移包应报错")
	}
}

func TestPlanRoundTrip(t *testing.T) {
	link, _ := crypto.NewLink("1.2.3.4", 8080)
	p := &Plan{Version: 1, Containers: []PlanContainer{{Name: "web", Ports: []PlanPort{{HostPort: "8080", ContainerPort: "80", Proto: "tcp"}}}}}
	var buf bytes.Buffer
	if err := EncryptPlan(&buf, link, p); err != nil {
		t.Fatal(err)
	}
	got, err := DecryptPlan(&buf, link)
	if err != nil || got.Containers[0].Ports[0].HostPort != "8080" {
		t.Fatalf("迁移计划：%+v %v", got, err)
	}
}

func TestValidName(t *testing.T) {
	ok := []string{"manifest.json", "volumes/site_data/part-000001", "networks/my-net.json", "compose/app/dir/part-000001",
		"volumes/3977206abb71f8aa4677d89b605e71e99ae3db6773688b6f62e61fbbadf9c34c.json"}
	bad := []string{"", "/abs", "a/../b", "..", ".hidden", "a//b", "a/", "空格 名字", "a\\b"}
	for _, n := range ok {
		if !ValidName(n) {
			t.Errorf("%q 应合规", n)
		}
	}
	for _, n := range bad {
		if ValidName(n) {
			t.Errorf("%q 不应合规", n)
		}
	}
}
