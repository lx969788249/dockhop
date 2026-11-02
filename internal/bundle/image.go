package bundle

import (
	"io"
	"runtime"

	"github.com/klauspost/compress/zstd"

	"github.com/lx969788249/dockhop/internal/crypto"
)

// WriteImageFile 把一个镜像的导出流（docker save 的输出）压缩、加密后写到 dst，
// 和迁移包用同样的压缩和加密。
func WriteImageFile(dst io.Writer, link crypto.Link, src io.Reader) error {
	enc, err := link.Encrypt(dst)
	if err != nil {
		return err
	}
	zw, err := zstd.NewWriter(enc,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(runtime.NumCPU()),
		zstd.WithWindowSize(4<<20))
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, src); err != nil {
		zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return enc.Close()
}

// OpenImageFile 解密、解压一个镜像文件，返回镜像的导出流，可以直接交给 docker load。
// 每一段密文都会先通过校验再交出去：文件被改过或不完整时读出错误，不会把坏数据交给 Docker。
func OpenImageFile(src io.Reader, link crypto.Link) (io.ReadCloser, error) {
	dr, err := link.Decrypt(src)
	if err != nil {
		return nil, err
	}
	zr, err := zstd.NewReader(dr, zstd.WithDecoderConcurrency(2), zstd.WithDecoderMaxWindow(16<<20), zstd.WithDecoderLowmem(true))
	if err != nil {
		return nil, err
	}
	return &imageReader{zr: zr}, nil
}

type imageReader struct{ zr *zstd.Decoder }

func (r *imageReader) Read(p []byte) (int, error) {
	n, err := r.zr.Read(p)
	if err != nil && err != io.EOF {
		err = wrapCorrupt(err)
	}
	return n, err
}

func (r *imageReader) Close() error {
	r.zr.Close()
	return nil
}
