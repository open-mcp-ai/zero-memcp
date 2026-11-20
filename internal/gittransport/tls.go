package gittransport

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// TLSConfig 描述与对端通信时的证书与信任约束。
//
// 为什么不用系统 CA 兜底：
// 两个 memory 节点之间同步，通常是跨机器、跨内网甚至是跨云的。
// 依赖公网 CA 既需要配域名，又容易被伪造证书劫持。
// 我们采用与 SSH host_key 一样的显式 Pin：直接比对对端证书的 SHA-256，
// 或者挂私有自签 CA，绝不做 InsecureSkipVerify。
type TLSConfig struct {
	// CAPath 指向受信任的私有 CA 或对端自签证书。
	CAPath string
	// Pin 是对端叶子证书 DER 的 SHA-256 指纹（hex，可含冒号），优先级高于 CA。
	Pin string
}

// NewTLSClient 构建一个带私有证书或指纹校验的 HTTP 客户端。
func NewTLSClient(cfg TLSConfig, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	tlsConf := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if cfg.Pin != "" {
		expectedPin := normalizePin(cfg.Pin)
		if len(expectedPin) != 64 {
			return nil, fmt.Errorf("tls_pin must be a 64-char hex SHA-256 fingerprint, got %q", cfg.Pin)
		}
		// 指纹校验接管证书链验证：
		tlsConf.InsecureSkipVerify = true
		tlsConf.VerifyPeerCertificate = func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("peer did not present any TLS certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			gotPin := hex.EncodeToString(sum[:])
			if gotPin != expectedPin {
				return fmt.Errorf("TLS certificate pin mismatch: expected %s, got %s", expectedPin, gotPin)
			}
			return nil
		}
	} else if cfg.CAPath != "" {
		caData, err := os.ReadFile(cfg.CAPath)
		if err != nil {
			return nil, fmt.Errorf("read tls_ca %s: %w", cfg.CAPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return nil, fmt.Errorf("no valid certificates found in tls_ca %s", cfg.CAPath)
		}
		tlsConf.RootCAs = pool
	}

	tr := &http.Transport{
		TLSClientConfig:       tlsConf,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func normalizePin(pin string) string {
	pin = strings.ToLower(strings.TrimSpace(pin))
	pin = strings.TrimPrefix(pin, "sha256:")
	pin = strings.ReplaceAll(pin, ":", "")
	return pin
}
