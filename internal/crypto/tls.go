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

// 下载服务用 HTTPS，两台服务器互相核对。旧服务器一般只有 IP，拿不到证书机构签发的证书，所以两边
// 的证书都是现场生成的，证书的密钥由链接里的密钥推算出来（旧服务器、新服务器各推算一个）。每一边都
// 用链接里的密钥算出对方的公钥，核对对方出示的证书，对得上才说明对方也有这条完整的链接：新服务器据此
// 确认连上的就是这次迁出的旧服务器，旧服务器据此只让有密钥的新服务器连接。链接不用多加任何东西。
// 只拿到链接 # 前面那部分（下载口令）的人连接都建立不起来，什么也下载不到；迁移包本身照旧用 age
// 加密（见 seal.go）。

// ErrWrongServer 表示连上的服务器出示的证书和链接里的密钥对不上：不是这次迁出的旧服务器。
var ErrWrongServer = errors.New("连上的服务器和链接里的密钥对不上")

// errWrongClient 表示连接下载服务的一方出示的证书和链接里的密钥对不上：它没有这条完整的链接。
// 只写进旧服务器的日志。
var errWrongClient = errors.New("连接的一方没有这次迁移的密钥")

// 推算两张证书的密钥用的标签：旧服务器的和新服务器的不一样，谁也冒充不了谁。
const (
	serverLabel = "dockhop-tls-v1"
	clientLabel = "dockhop-tls-client-v1"
)

// tlsKey 是证书的密钥，由链接里的密钥按 label 推算出来：两台服务器各自算，结果一样。
func (l Link) tlsKey(label string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte(label + "\x00" + l.key()))
	return ed25519.NewKeyFromSeed(seed[:])
}

// certificate 现场生成一张证书，密钥由链接里的密钥按 label 推算。
func (l Link) certificate(label string, usage x509.ExtKeyUsage) (*tls.Certificate, error) {
	key := l.tlsKey(label)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial.Add(serial, big.NewInt(1)), // 必须是正数
		Subject:      pkix.Name{CommonName: "DockHop"},
		// 对方只核对公钥、不看有效期，这里的时间只是证书格式要求要有。
		NotBefore:   now.Add(-24 * time.Hour),
		NotAfter:    now.AddDate(1, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// verifyPeer 核对对方出示的证书的公钥是不是由链接里的密钥按 label 推算出来的，对不上时返回 wrong。
// 握手时对方还要用这个公钥对应的私钥签名，只有知道链接里的密钥才算得出私钥。
func (l Link) verifyPeer(label string, wrong error) func(tls.ConnectionState) error {
	want := l.tlsKey(label).Public().(ed25519.PublicKey)
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) > 0 {
			if pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey); ok && pub.Equal(want) {
				return nil
			}
		}
		return wrong
	}
}

// ServerTLS 是旧服务器下载服务的 TLS 设置：出示旧服务器的证书（每次现场生成），并要求连接的一方
// 出示新服务器的证书，按链接核对，对不上就在握手时断开。
func (l Link) ServerTLS() (*tls.Config, error) {
	cert, err := l.certificate(serverLabel, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:       tls.VersionTLS13,
		Certificates:     []tls.Certificate{*cert},
		ClientAuth:       tls.RequireAnyClientCert, // 不按证书机构核对，由 VerifyConnection 按链接核对
		VerifyConnection: l.verifyPeer(clientLabel, errWrongClient),
	}, nil
}

// ClientTLS 是新服务器连接旧服务器用的 TLS 设置：不按证书机构和域名核对（旧服务器的证书没有
// 机构签发），改为按链接核对旧服务器的证书；旧服务器要求时出示新服务器的证书（每次现场生成）。
func (l Link) ClientTLS() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // 不按证书机构核对，由 VerifyConnection 按链接核对
		VerifyConnection:   l.verifyPeer(serverLabel, ErrWrongServer),
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return l.certificate(clientLabel, x509.ExtKeyUsageClientAuth)
		},
	}
}
