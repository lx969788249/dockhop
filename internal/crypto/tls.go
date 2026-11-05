package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"time"
)

// 下载服务用 HTTPS。旧服务器一般只有 IP，拿不到证书机构签发的证书，所以证书是迁出时现场
// 生成的，证书的密钥由链接里的密钥推算出来；新服务器用链接里的密钥算出同一个公钥，核对旧服务器
// 出示的证书，对得上才说明连上的就是这次迁出的旧服务器。链接不用多加任何东西。没有链接的人
// 冒充不了旧服务器，也看不到下载口令和迁入结果；迁移包本身照旧用 age 加密（见 seal.go）。

// ErrWrongServer 表示连上的服务器出示的证书和链接里的密钥对不上：不是这次迁出的旧服务器。
var ErrWrongServer = errors.New("连上的服务器和链接里的密钥对不上")

// tlsKey 是下载服务证书的密钥，由链接里的密钥推算出来：两台服务器各自算，结果一样。
func (l Link) tlsKey() ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("dockhop-tls-v1\x00" + l.key()))
	return ed25519.NewKeyFromSeed(seed[:])
}

// ServerTLS 是旧服务器下载服务的 TLS 设置，证书每次现场生成。
func (l Link) ServerTLS() (*tls.Config, error) {
	key := l.tlsKey()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial.Add(serial, big.NewInt(1)), // 必须是正数
		Subject:      pkix.Name{CommonName: "DockHop"},
		// 新服务器只核对公钥、不看有效期，这里的时间只是证书格式要求要有。
		NotBefore:   now.Add(-24 * time.Hour),
		NotAfter:    now.AddDate(1, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}, nil
}

// ClientTLS 是新服务器连接旧服务器用的 TLS 设置：不按证书机构和域名核对（旧服务器的证书没有
// 机构签发），改为核对证书的公钥是不是由链接里的密钥推算出来的。握手时旧服务器还要用这个公钥
// 对应的私钥签名，只有知道链接里的密钥才算得出私钥。
func (l Link) ClientTLS() *tls.Config {
	want := l.tlsKey().Public().(ed25519.PublicKey)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // 不按证书机构核对，由 VerifyConnection 按链接核对
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) > 0 {
				if pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey); ok && pub.Equal(want) {
					return nil
				}
			}
			return ErrWrongServer
		},
	}
}
