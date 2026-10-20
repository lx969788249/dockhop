package bundle

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/lx969788249/dockhop/internal/crypto"
)

// 迁移包里只允许这样的文件名：由字母、数字和 ._@+=- 组成的若干段，没有 ..、
// 没有绝对路径，解开时不可能写到目标目录以外。
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_@+=-][A-Za-z0-9._@+=-]*(/[A-Za-z0-9_@+=-][A-Za-z0-9._@+=-]*)*$`)

// ValidName 表示文件名是否可以放进迁移包。
func ValidName(name string) bool {
	if !namePattern.MatchString(name) {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// ErrUnsafe 表示迁移包里有不合规的内容，拒绝解开。
var ErrUnsafe = errors.New("迁移包里有不合规的内容，已拒绝解开")

// Extract 解密、解压迁移包，把内容解到 dst 目录。每一段数据都会先通过校验再写入；
// 只接受普通文件，文件名必须合规。progress 不为 nil 时会收到读入的密文字节数。
func Extract(src io.Reader, link crypto.Link, dst string, progress io.Writer) error {
	if progress != nil {
		src = io.TeeReader(src, progress)
	}
	dr, err := link.Decrypt(src)
	if err != nil {
		return err
	}
	zr, err := zstd.NewReader(dr, zstd.WithDecoderConcurrency(2), zstd.WithDecoderMaxWindow(16<<20), zstd.WithDecoderLowmem(true))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return wrapCorrupt(err)
		}
		if h.Typeflag != tar.TypeReg || !ValidName(h.Name) {
			return fmt.Errorf("%w：%q", ErrUnsafe, h.Name)
		}
		p := filepath.Join(dst, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return wrapCorrupt(err)
		}
	}
}

func wrapCorrupt(err error) error {
	if errors.Is(err, crypto.ErrCorrupted) || errors.Is(err, os.ErrNotExist) {
		return err
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return err // 磁盘写入出错，不是迁移包的问题
	}
	return fmt.Errorf("%w（%v）", crypto.ErrCorrupted, err)
}

// ReadManifest 读出解开后的清单，并确认是这个版本能读懂的迁移包。
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("迁移包不完整：没有清单（%v）", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("迁移包的清单读不懂：%v", err)
	}
	if m.Format != Format {
		return nil, errors.New("这不是 DockHop 的迁移包")
	}
	if m.Version > Version {
		return nil, fmt.Errorf("这个迁移包是更新版本的 DockHop 生成的（格式版本 %d），请把新服务器上的 DockHop 也更新到最新版", m.Version)
	}
	return &m, nil
}

// OpenStream 把 prefix 下的分段按顺序拼回一条数据流。
func OpenStream(dir, prefix string, parts int) io.ReadCloser {
	readers := make([]io.Reader, 0, parts)
	files := make([]*os.File, 0, parts)
	var openErr error
	for i := 1; i <= parts; i++ {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(PartName(prefix, i))))
		if err != nil {
			openErr = fmt.Errorf("迁移包不完整：缺少 %s（%v）", PartName(prefix, i), err)
			break
		}
		files = append(files, f)
		readers = append(readers, f)
	}
	return &multiCloser{r: io.MultiReader(readers...), files: files, err: openErr}
}

type multiCloser struct {
	r     io.Reader
	files []*os.File
	err   error
}

func (m *multiCloser) Read(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	return m.r.Read(p)
}

func (m *multiCloser) Close() error {
	for _, f := range m.files {
		f.Close()
	}
	return nil
}
