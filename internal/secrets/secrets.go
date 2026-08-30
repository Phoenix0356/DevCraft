// Package secrets 负责敏感配置（LLM API Key）的落盘加密。
// 方案：AES-256-GCM（认证加密，防篡改），密钥是应用数据目录下一个
// 随机生成的 32 字节文件 secret.key（权限 0600，仅当前用户可读）。
// 类比 Java：相当于用本地文件当 KeyStore 的简化版。
package secrets

import (
	"crypto/aes"    // AES 分组密码原语
	"crypto/cipher" // GCM 加密模式
	"crypto/rand"   // 密码学安全随机数
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

// Box 是加解密器（持有密钥）。
type Box struct {
	key []byte // 32 字节 = AES-256
}

// NewBox 打开（不存在则创建）dir 下的密钥文件并返回加解密器。
func NewBox(dir string) (*Box, error) {
	keyFile := filepath.Join(dir, "secret.key")
	data, err := os.ReadFile(keyFile)
	if err == nil && len(data) == 32 { // 已有合法密钥，直接用
		return &Box{key: data}, nil
	}
	if err != nil && !os.IsNotExist(err) { // 非"文件不存在"的读错误才上报
		return nil, err
	}
	// 首次运行：生成 32 字节随机密钥
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil { // rand.Read 用操作系统熵源填充字节片
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { // 0700：仅属主可读写执行
		return nil, err
	}
	// 0600：仅属主可读写——密钥文件的基本卫生要求
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}
	return &Box{key: key}, nil
}

// Encrypt 加密明文，返回 base64 字符串（可直接存 SQLite 的 TEXT 列）。
func (b *Box) Encrypt(plain string) (string, error) {
	block, err := aes.NewCipher(b.key) // 按密钥长度选择 AES-128/192/256
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block) // GCM 模式：加密 + 完整性认证一体
	if err != nil {
		return "", err
	}
	// 每次加密必须使用全新的随机 nonce（GCM 的铁律：同 key 复用 nonce 会泄露明文）
	nonce := make([]byte, gcm.NonceSize()) // 通常 12 字节
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	// Seal(dst, nonce, plaintext, 附加认证数据)：输出 = nonce 前缀 + 密文 + 认证 tag
	// 这里把 nonce 放在密文前面一起输出，解密时先切出来
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 是 Encrypt 的逆过程。
func (b *Box) Decrypt(enc string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(b.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() { // 数据太短必然非法，提前失败
		return "", fmt.Errorf("ciphertext too short")
	}
	// 切出 nonce 前缀，其余是密文+tag
	nonce, sealed := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	// Open 同时完成解密与完整性校验：数据被篡改或密钥不对都会返回错误
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
