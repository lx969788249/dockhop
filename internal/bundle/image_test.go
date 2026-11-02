package bundle

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/lx969788249/dockhop/internal/crypto"
)

// 镜像文件：压缩加密后能原样读回；被改动、截断或密钥不对时读出错误，不会交出坏数据。
func TestImageFileRoundTrip(t *testing.T) {
	link, _ := crypto.NewLink("1.2.3.4", 8080)
	data := make([]byte, 300<<10) // 比一段密文（64KB）大，确认分段校验
	rand.Read(data[:100<<10])     // 一部分随机（压不动），一部分全是 0（压得很小）
	var sealed bytes.Buffer
	if err := WriteImageFile(&sealed, link, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if sealed.Len() >= len(data) {
		t.Errorf("应压缩：%d → %d", len(data), sealed.Len())
	}
	read := func(b []byte, l crypto.Link) ([]byte, error) {
		rc, err := OpenImageFile(bytes.NewReader(b), l)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	got, err := read(sealed.Bytes(), link)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("读回的内容不对：%v", err)
	}

	tampered := append([]byte(nil), sealed.Bytes()...)
	tampered[len(tampered)/2] ^= 0xff
	if _, err := read(tampered, link); !errors.Is(err, crypto.ErrCorrupted) {
		t.Errorf("被改动的镜像文件应报 ErrCorrupted：%v", err)
	}
	if _, err := read(sealed.Bytes()[:sealed.Len()-100], link); !errors.Is(err, crypto.ErrCorrupted) {
		t.Errorf("不完整的镜像文件应报 ErrCorrupted：%v", err)
	}
	other, _ := crypto.NewLink("1.2.3.4", 8080)
	if _, err := read(sealed.Bytes(), other); !errors.Is(err, crypto.ErrWrongKey) {
		t.Errorf("密钥不对应报 ErrWrongKey：%v", err)
	}
}
