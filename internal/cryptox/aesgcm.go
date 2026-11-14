// Package cryptox 提供 AES-256-GCM 分段加解密。
//
// nonce 为 12 字节：前 4 字节是 big-endian epoch，后 8 字节是 big-endian
// 段号。同一密钥（epoch）下段号唯一，因此同一 (key, nonce) 组合永不重复，
// 满足 GCM 对 nonce 唯一性的硬性要求。
//
// AAD 以固定分隔符拼接段号、key_id 与清单头摘要，密文解密前必须重新比对，
// 防止段换位、密钥标识错配与清单被整体替换。
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
)

const nonceSize = 12

// ErrTagInvalid 表示 GCM 认证标签校验失败（密文被篡改）。
var ErrTagInvalid = errors.New("TAG_INVALID")

// ErrAADMismatch 表示调用方提供的 AAD 与解密参数不一致（段换位或 key_id 不符）。
var ErrAADMismatch = errors.New("AAD_MISMATCH")

// DeriveNonce 由 epoch 与段号确定性派生 12 字节 nonce。
func DeriveNonce(epoch, segmentIndex int64) []byte {
	nonce := make([]byte, nonceSize)
	binary.BigEndian.PutUint32(nonce[:4], uint32(epoch))
	binary.BigEndian.PutUint64(nonce[4:], uint64(segmentIndex))
	return nonce
}

// BuildAAD 构造附加认证数据。digest 为清单头摘要（调用方按 hex 传入）。
// 字段顺序固定，使用竖线分隔，数值与 key_id 不可能包含竖线，无歧义。
func BuildAAD(segmentIndex int64, keyID string, digestHex string) []byte {
	return []byte(fmt.Sprintf("seg=%d|kid=%s|manifest=%s", segmentIndex, keyID, digestHex))
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal 加密单段明文，返回 GCM 标准输出（密文与 16 字节标签拼接在一起）。
func Seal(key, nonce, plaintext, aad []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

// Open 认证并解密单段。调用方必须先保证 nonce、段号、key_id、摘要与清单
// 记录一致，否则返回 ErrAADMismatch；标签校验失败返回 ErrTagInvalid。
func Open(key, nonce, ciphertext, aad []byte, expectedSegment int64, expectedKeyID, digestHex string) ([]byte, error) {
	if len(nonce) != nonceSize {
		return nil, fmt.Errorf("%w: bad nonce length %d", ErrAADMismatch, len(nonce))
	}
	gotEpoch := int64(binary.BigEndian.Uint32(nonce[:4]))
	gotIndex := int64(binary.BigEndian.Uint64(nonce[4:]))
	if gotIndex != expectedSegment {
		return nil, fmt.Errorf("%w: nonce segment %d != manifest %d", ErrAADMismatch, gotIndex, expectedSegment)
	}
	expectedNonce := DeriveNonce(gotEpoch, expectedSegment)
	if string(expectedNonce) != string(nonce) {
		return nil, fmt.Errorf("%w: nonce not derived from epoch/segment", ErrAADMismatch)
	}
	expectedAAD := BuildAAD(expectedSegment, expectedKeyID, digestHex)
	if string(expectedAAD) != string(aad) {
		return nil, fmt.Errorf("%w: aad binding differs", ErrAADMismatch)
	}
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrTagInvalid
	}
	return plaintext, nil
}
