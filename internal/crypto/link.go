// Package crypto 负责迁移链接和加密。
//
// 链接形如 https://203.0.113.7:8080/k3Jx9QmP2vLa/#a7j8…（约 100 个字符）：
// 路径里是随机的下载口令，# 后面是密钥。# 后面的部分浏览器和 curl 都不会发给
// 服务器，所以下载服务本身拿不到密钥。迁移包和迁移计划都用这个密钥加密，
// 加密方式是开源加密库 age 的分块认证加密：每 64KB 一段，每段都带校验，
// 能发现内容被改、被截断或顺序被调换。传输还走 HTTPS，证书由同一个密钥推算
// （见 tls.go）。
package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"filippo.io/age"
)

const (
	keyPrefix   = "AGE-SECRET-KEY-1"
	tokenLength = 12
)

var (
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)
	keyPattern   = regexp.MustCompile(`^[0-9a-z]{58}$`)
)

// LinkError 说明链接哪里不对。
type LinkError struct{ Reason string }

func (e *LinkError) Error() string { return e.Reason }

// Link 是一条迁移链接。
type Link struct {
	Host  string // 地址，IPv6 不带方括号
	Port  int
	Token string
	id    *age.X25519Identity
}

// NewLink 为一次迁出生成新的口令和密钥。
func NewLink(host string, port int) (Link, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return Link{}, err
	}
	token, err := randomToken(tokenLength)
	if err != nil {
		return Link{}, err
	}
	return Link{Host: host, Port: port, Token: token, id: id}, nil
}

func randomToken(n int) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	b := make([]byte, n)
	for i := range b {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b), nil
}

// key 是写在链接 # 后面的密钥：age 私钥去掉固定前缀后的小写形式。
func (l Link) key() string {
	return strings.ToLower(strings.TrimPrefix(l.id.String(), keyPrefix))
}

// Secret 返回密钥，用于在日志里隐藏它。
func (l Link) Secret() string { return l.key() }

// Base 是下载地址的公共前缀，例如 https://1.2.3.4:8080/k3Jx9QmP2vLa/。
func (l Link) Base() string {
	host := l.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("https://%s:%d/%s/", host, l.Port, l.Token)
}

// String 返回完整的迁移链接（含密钥）。只能显示给用户，不能写进日志。
func (l Link) String() string { return l.Base() + "#" + l.key() }

// BundleURL、PlanURL、StatusURL 是同一条链接下的三个地址：迁移包、迁移计划、
// 迁入完成后通知旧服务器的地址。
func (l Link) BundleURL() string { return l.Base() + "bundle" }
func (l Link) PlanURL() string   { return l.Base() + "plan" }
func (l Link) StatusURL() string { return l.Base() + "status" }

// ImageURL 是单独提供的一个镜像文件的下载地址。
func (l Link) ImageURL(name string) string { return l.Base() + "image/" + name }

// ParseLink 解析用户粘贴的链接，出错时用大白话说明哪里不对。
func ParseLink(s string) (Link, error) {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	if s == "" {
		return Link{}, &LinkError{"没有提供迁移链接"}
	}
	u, err := url.Parse(s)
	if err == nil && u.Scheme == "http" && u.Host != "" {
		return Link{}, &LinkError{"这个链接是旧版本的 DockHop 生成的（以 http:// 开头），请在旧服务器上重新运行迁出，用新显示的链接"}
	}
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return Link{}, &LinkError{"这不是迁移链接。请完整复制旧服务器上显示的、以 https:// 开头的链接"}
	}
	port := 443
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Link{}, &LinkError{"链接里的端口不对：" + p}
		}
		port = n
	}
	token := strings.Trim(u.Path, "/")
	if !tokenPattern.MatchString(token) {
		return Link{}, &LinkError{"链接里的下载地址不完整，请完整复制旧服务器上显示的链接"}
	}
	frag := u.Fragment
	if frag == "" {
		return Link{}, &LinkError{"链接缺少 # 后面的密钥，请完整复制旧服务器上显示的链接（包括 # 和后面的部分）"}
	}
	if !keyPattern.MatchString(frag) {
		return Link{}, &LinkError{"链接里的密钥不完整，可能没有复制全，请重新完整复制"}
	}
	id, err := age.ParseX25519Identity(keyPrefix + strings.ToUpper(frag))
	if err != nil {
		return Link{}, &LinkError{"链接里的密钥不对，可能复制时改动了字符，请重新完整复制"}
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	return Link{Host: host, Port: port, Token: token, id: id}, nil
}

// SignStatus 用链接里的密钥给迁入结果签名：只有拿到完整链接的新服务器才能通知
// 旧服务器“迁入完成”，别人伪造不了。
func (l Link) SignStatus(body []byte) []byte {
	key := sha256.Sum256([]byte("dockhop-status-v1\x00" + l.key()))
	m := hmac.New(sha256.New, key[:])
	m.Write(body)
	return m.Sum(nil)
}

// VerifyStatus 检查迁入结果的签名。
func (l Link) VerifyStatus(body, sig []byte) bool {
	return hmac.Equal(l.SignStatus(body), sig)
}
