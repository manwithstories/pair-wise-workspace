// Package manifest 管理加密清单：每段的文件偏移、密文长度、key_id、epoch、
// nonce 与 GCM 标签。清单整体使用 HMAC-SHA256 防篡改，签名密钥由主密钥按
// 专用 label 派生，明文主密钥不写入任何文件。
package manifest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"kmsctl/internal/keyschedule"
)

// ErrTamper 表示清单 HMAC 校验失败，内容被篡改或主密钥不匹配。
var ErrTamper = errors.New("MANIFEST_TAMPER")

// Header 描述整份课件的加密参数，其摘要被逐段绑定进 AAD。
type Header struct {
	Version      int    `json:"version"`
	Algorithm    string `json:"algorithm"`
	SegmentSize  int    `json:"segment_size"`
	RotateEvery  int    `json:"rotate_every"`
	EpochSeconds int64  `json:"epoch_seconds"`
	SegmentCount int    `json:"segment_count"`
	CipherFile   string `json:"cipher_file"`
	MasterKeyID  string `json:"master_key_id"`
}

// SegmentRecord 描述单段密文在输出文件中的位置与所用密钥材料。
type SegmentRecord struct {
	Index        int    `json:"index"`
	Offset       int64  `json:"offset"`
	CipherLength int    `json:"cipher_length"`
	PlainLength  int    `json:"plain_length"`
	KeyID        string `json:"key_id"`
	Epoch        int64  `json:"epoch"`
	Nonce        string `json:"nonce"`
	Tag          string `json:"tag"`
}

// Manifest 是清单文件的完整结构。
type Manifest struct {
	Header   Header          `json:"header"`
	Segments []SegmentRecord `json:"segments"`
	Sig      string          `json:"sig"`
}

// Digest 对清单头做确定性 SHA-256 摘要，返回 hex。摘要进入每段 AAD，使任意
// 全局参数（段大小、轮换参数、密文文件名等）被替换都会导致解密失败。
func (m *Manifest) Digest() string {
	raw, _ := json.Marshal(m.Header)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func hmacKey(masterKey []byte) []byte {
	return keyschedule.DeriveSecret(masterKey, "manifest-hmac", 0)
}

func signingBytes(m *Manifest) []byte {
	copy := Manifest{Header: m.Header, Segments: m.Segments}
	raw, _ := json.Marshal(copy)
	return raw
}

// Sign 计算并写入清单 HMAC。
func (m *Manifest) Sign(masterKey []byte) {
	mac := hmac.New(sha256.New, hmacKey(masterKey))
	mac.Write(signingBytes(m))
	m.Sig = hex.EncodeToString(mac.Sum(nil))
}

// Verify 校验清单 HMAC，失败返回 ErrTamper。
func (m *Manifest) Verify(masterKey []byte) error {
	want, err := hex.DecodeString(m.Sig)
	if err != nil {
		return fmt.Errorf("%w: bad signature encoding", ErrTamper)
	}
	mac := hmac.New(sha256.New, hmacKey(masterKey))
	mac.Write(signingBytes(m))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return ErrTamper
	}
	return nil
}

// Save 先签名再以确定性 JSON 落盘（缩进保证可审阅，字段序由结构体固定）。
func (m *Manifest) Save(path string, masterKey []byte) error {
	m.Sign(masterKey)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// Load 读取清单文件并校验 HMAC。
func Load(path string, masterKey []byte) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTamper, err)
	}
	if err := m.Verify(masterKey); err != nil {
		return nil, err
	}
	return &m, nil
}

// MasterKeyID 由主密钥指纹前 8 字节生成，仅用于标识，不泄露密钥材料。
func MasterKeyID(masterKey []byte) string {
	fp := keyschedule.KeyFingerprint(masterKey)
	return hex.EncodeToString(fp[:8])
}
