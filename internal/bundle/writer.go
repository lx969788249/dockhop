package bundle

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/lx969788249/dockhop/internal/crypto"
)

// Writer 把内容写进迁移包。写完必须调用 Close，否则迁移包不完整、无法解开。
type Writer struct {
	enc  io.WriteCloser
	zw   *zstd.Encoder
	tw   *tar.Writer
	buf  []byte
	seen map[string]bool
	now  time.Time
}

// NewWriter 开始写一个迁移包到 dst：tar → zstd 压缩 → age 加密。
func NewWriter(dst io.Writer, link crypto.Link) (*Writer, error) {
	enc, err := link.Encrypt(dst)
	if err != nil {
		return nil, err
	}
	zw, err := zstd.NewWriter(enc,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(runtime.NumCPU()),
		zstd.WithWindowSize(4<<20)) // 窗口不大，新服务器解压时内存占用也小
	if err != nil {
		return nil, err
	}
	return &Writer{enc: enc, zw: zw, tw: tar.NewWriter(zw), seen: map[string]bool{}, now: time.Now()}, nil
}

func (w *Writer) header(name string, size int64) error {
	if !ValidName(name) {
		return fmt.Errorf("迁移包里的文件名不合规：%q", name)
	}
	if w.seen[name] {
		return fmt.Errorf("迁移包里有重复的文件：%s", name)
	}
	w.seen[name] = true
	return w.tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o600, ModTime: w.now, Format: tar.FormatPAX,
	})
}

// AddBytes 写入一个文件。
func (w *Writer) AddBytes(name string, b []byte) error {
	if err := w.header(name, int64(len(b))); err != nil {
		return err
	}
	_, err := w.tw.Write(b)
	return err
}

// AddJSON 把 v 编码成 JSON 写入一个文件。已经是 JSON 的内容（json.RawMessage、
// []byte）原样写入，不再编码。
func (w *Writer) AddJSON(name string, v any) error {
	switch raw := v.(type) {
	case json.RawMessage:
		return w.AddBytes(name, raw)
	case []byte:
		return w.AddBytes(name, raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return w.AddBytes(name, b)
}

// AddStream 把一条事先不知道长度的数据流分段写成 prefix/part-000001……，返回段数和总字节数。
func (w *Writer) AddStream(prefix string, r io.Reader) (int, int64, error) {
	if w.buf == nil {
		w.buf = make([]byte, PartSize)
	}
	parts := 0
	var total int64
	for {
		n, err := io.ReadFull(r, w.buf)
		if n > 0 {
			parts++
			if herr := w.AddBytes(PartName(prefix, parts), w.buf[:n]); herr != nil {
				return parts, total, herr
			}
			total += int64(n)
		}
		switch err {
		case nil:
			continue
		case io.EOF, io.ErrUnexpectedEOF:
			return parts, total, nil
		default:
			return parts, total, err
		}
	}
}

// Close 写完迁移包：结束 tar、压缩和加密。
func (w *Writer) Close() error {
	if err := w.tw.Close(); err != nil {
		return err
	}
	if err := w.zw.Close(); err != nil {
		return err
	}
	return w.enc.Close()
}

// PartName 是第 n 段的文件名。
func PartName(prefix string, n int) string { return fmt.Sprintf("%s/part-%06d", prefix, n) }
