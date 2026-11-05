package crypto

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

func TestLinkRoundTrip(t *testing.T) {
	for _, host := range []string{"203.0.113.7", "2001:db8::1", "example.com"} {
		l, err := NewLink(host, 8080)
		if err != nil {
			t.Fatal(err)
		}
		s := l.String()
		if !strings.HasPrefix(s, "https://") {
			t.Errorf("链接应以 https:// 开头：%s", s)
		}
		if len(s) > 110 {
			t.Errorf("链接太长（%d 个字符）：%s", len(s), s)
		}
		if strings.Contains(host, ":") && !strings.Contains(s, "["+host+"]") {
			t.Errorf("IPv6 地址应加方括号：%s", s)
		}
		got, err := ParseLink("  '" + s + "'\n") // 用户可能连引号和空白一起粘贴
		if err != nil {
			t.Fatalf("解析自己生成的链接失败：%v（%s）", err, s)
		}
		if got.String() != s || got.Host != host || got.Port != 8080 || got.Token != l.Token {
			t.Errorf("解析结果不一致：%+v", got)
		}
		if got.BundleURL() != l.Base()+"bundle" || got.PlanURL() != l.Base()+"plan" || got.StatusURL() != l.Base()+"status" {
			t.Errorf("三个地址不对：%s %s %s", got.BundleURL(), got.PlanURL(), got.StatusURL())
		}
	}
}

func TestParseLinkErrors(t *testing.T) {
	l, _ := NewLink("1.2.3.4", 8080)
	good := l.String()
	// 只改密钥里的一个字符（校验码会对不上）；不能用字符串替换，同样的字符可能出现在链接前面。
	key := []byte(l.Secret())
	if key[10] == 'q' {
		key[10] = 'p'
	} else {
		key[10] = 'q'
	}
	cases := []struct {
		in   string
		want string
	}{
		{"", "没有提供迁移链接"},
		{"ftp://1.2.3.4/abc", "这不是迁移链接"},
		{"1.2.3.4:8080", "这不是迁移链接"},
		{l.Base(), "缺少 # 后面的密钥"},
		{good[:len(good)-5], "密钥不完整"},
		{"https://1.2.3.4:8080/x/#" + l.Secret(), "下载地址不完整"},
		{"https://1.2.3.4:99999/" + l.Token + "/#" + l.Secret(), "端口不对"},
		// 1.2.0 之前的版本生成的 http:// 链接不再支持：提示在旧服务器上重新迁出。
		{strings.Replace(good, "https://", "http://", 1), "旧版本的 DockHop 生成的"},
		{l.Base() + "#" + string(key), "密钥不对"},
	}
	for _, c := range cases {
		_, err := ParseLink(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseLink(%q) 应提示「%s」，实际：%v", c.in, c.want, err)
		}
	}
}

func seal(t *testing.T, l Link, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := l.Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func open(l Link, sealed []byte) ([]byte, error) {
	r, err := l.Decrypt(bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestEncryptDecrypt(t *testing.T) {
	l, _ := NewLink("1.2.3.4", 8080)
	plain := make([]byte, 1<<20+123) // 跨越多个 64KB 分段
	rand.Read(plain)
	sealed := seal(t, l, plain)

	// 用链接解析出来的密钥（新服务器的情形）能解开。
	parsed, _ := ParseLink(l.String())
	got, err := open(parsed, sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("解密结果不一致：%v", err)
	}

	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)/2] ^= 1
	if _, err := open(l, tampered); !errors.Is(err, ErrCorrupted) {
		t.Errorf("内容被改动时应返回 ErrCorrupted：%v", err)
	}
	if _, err := open(l, sealed[:len(sealed)-100]); !errors.Is(err, ErrCorrupted) {
		t.Errorf("内容被截断时应返回 ErrCorrupted：%v", err)
	}
	other, _ := NewLink("1.2.3.4", 8080)
	if _, err := open(other, sealed); !errors.Is(err, ErrWrongKey) {
		t.Errorf("密钥不对时应返回 ErrWrongKey：%v", err)
	}
}

// handshake 让 server 链接的下载服务和拿着 client 链接的新服务器握手，返回新服务器那边的结果。
func handshake(t *testing.T, server, client Link) error {
	t.Helper()
	cfg, err := server.ServerTLS()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s := tls.Server(c, cfg)
		_ = s.Handshake()
		s.Close()
	}()
	c, err := tls.Dial("tcp", ln.Addr().String(), client.ClientTLS())
	if err == nil {
		c.Close()
	}
	<-done
	return err
}

// 新服务器按链接里的密钥核对旧服务器的证书：同一条链接（旧服务器生成的、新服务器粘贴解析的）
// 握手成功；别的链接的下载服务冒充不了。证书每次现场生成，内容不同也不影响核对。
func TestTLSIdentity(t *testing.T) {
	l, _ := NewLink("127.0.0.1", 8080)
	parsed, err := ParseLink(l.String())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := handshake(t, l, parsed); err != nil {
			t.Fatalf("同一条链接应握手成功：%v", err)
		}
	}
	other, _ := NewLink("127.0.0.1", 8080)
	if err := handshake(t, other, parsed); !errors.Is(err, ErrWrongServer) {
		t.Fatalf("别的链接的下载服务应被认出来：%v", err)
	}
}
