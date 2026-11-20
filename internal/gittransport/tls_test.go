package gittransport

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 生成测试专用的自签证书并返回其 tls.Certificate 与 SHA-256 指纹。
func makeTestSelfSigned(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"zero-memcp test"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	pin := hex.EncodeToString(sum[:])

	cert := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}
	return cert, pin
}

func TestTLSClientWithFingerprintPin(t *testing.T) {
	cert, pin := makeTestSelfSigned(t)

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secure pong"))
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	defer ts.Close()

	// 1. 正确指纹：顺利握手通过
	client, err := NewTLSClient(TLSConfig{Pin: pin}, 5*time.Second)
	if err != nil {
		t.Fatalf("NewTLSClient: %v", err)
	}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("带正确 pin 的 HTTPS 请求应当成功，实际报错: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码不对: %d", resp.StatusCode)
	}

	// 2. 错误指纹（或者伪造中间人证书）：必须当场拒绝
	wrongPin := strings.Repeat("0", 64)
	badClient, err := NewTLSClient(TLSConfig{Pin: wrongPin}, 5*time.Second)
	if err != nil {
		t.Fatalf("NewTLSClient wrong pin: %v", err)
	}
	_, err = badClient.Get(ts.URL)
	if err == nil {
		t.Fatal("指纹不匹配时应当拒绝握手，实际却成功了")
	}
	if !strings.Contains(err.Error(), "pin mismatch") {
		t.Fatalf("报错应指出 pin 不匹配，实际报错: %v", err)
	}
}
