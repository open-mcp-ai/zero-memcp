package auth

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// SecretSize 是 HMAC-SHA256 密钥的字节数。32 字节 = 256 位熵，够用。
const SecretSize = 32

// LoadOrCreateSecret 读取签名密钥，不存在则生成一次并落盘。
//
// 为什么必须落盘：每次启动重新生成会让所有已签发的 token 同时失效，
// 而这种故障的表现是「所有设备一起 401」——看到这个现象的人会先怀疑
// 网络、再怀疑配置，最后才想到密钥，白白浪费半天。
//
// 为什么存原始字节而不是口令：这把密钥不需要人类记住，它是机器自己
// 生成、自己读的。原始字节就是最高熵的形态，不需要 KDF、不需要 base64。
//
// 为什么 tmp + rename：第一次启动时崩在中间会留下半截文件，那个文件
// 长度不是 32，之后每次启动都会报「期望 32 字节」，而删掉它意味着
// 所有已发出的 token 全部作废。rename 是原子的，不会留下半截状态。
func LoadOrCreateSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != SecretSize {
			return nil, fmt.Errorf("%s: 期望 %d 字节，实际 %d（该文件不可手改）",
				path, SecretSize, len(b))
		}
		return b, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读密钥 %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("建目录 %s: %w", filepath.Dir(path), err)
	}

	secret := make([]byte, SecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("生成密钥: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, secret, 0o600); err != nil {
		return nil, fmt.Errorf("写密钥 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("提交密钥 %s: %w", path, err)
	}
	return secret, nil
}
