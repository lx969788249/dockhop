package crypto

import (
	"errors"
	"fmt"
	"io"

	"filippo.io/age"
)

var (
	// ErrWrongKey 表示密钥和迁移包对不上：链接不是这个迁移包的，或者复制错了。
	ErrWrongKey = errors.New("链接里的密钥和迁移包对不上，请确认用的是旧服务器这次显示的链接")
	// ErrCorrupted 表示迁移包内容校验失败：传输中损坏、被改动或不完整。
	ErrCorrupted = errors.New("迁移包内容校验失败：文件在传输中损坏、被改动或不完整")
)

// Encrypt 返回一个写入端：写进去的明文会被加密后写到 dst。必须调用 Close
// 写入最后一段，否则迁移包不完整。
func (l Link) Encrypt(dst io.Writer) (io.WriteCloser, error) {
	return age.Encrypt(dst, l.id.Recipient())
}

// Decrypt 返回解密后的读取端。每一段读出之前都会先校验；内容不对时读取会返回
// ErrCorrupted，不会把未经校验的数据交给调用方。
func (l Link) Decrypt(src io.Reader) (io.Reader, error) {
	r, err := age.Decrypt(src, l.id)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, ErrWrongKey
		}
		return nil, fmt.Errorf("%w（%v）", ErrCorrupted, err)
	}
	return &checkedReader{r: r}, nil
}

type checkedReader struct{ r io.Reader }

func (c *checkedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("%w（%v）", ErrCorrupted, err)
	}
	return n, err
}
