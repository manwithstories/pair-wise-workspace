package adapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"io"
	"os"

	"github.com/disintegration/imaging"
)

// LoadImage 在 root 下安全加载图像，返回解码图像与内容 SHA-256（hex）。
func LoadImage(root, rel string, maxBytes int64) (image.Image, string, error) {
	data, err := ReadSecure(root, rel, maxBytes)
	if err != nil {
		return nil, "", err
	}
	img, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("adapter: decode %q: %w", rel, err)
	}
	sum := sha256.Sum256(data)
	return img, hex.EncodeToString(sum[:]), nil
}

// VerifySHA256 校验 root 下相对路径文件的 SHA-256 是否匹配。
func VerifySHA256(root, rel, wantHex string, maxBytes int64) (bool, error) {
	data, err := ReadSecure(root, rel, maxBytes)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == wantHex, nil
}

// ReadSecure 在 root 内按上限读取相对路径文件。
func ReadSecure(root, rel string, maxBytes int64) ([]byte, error) {
	full, err := SecureJoin(root, rel)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = MaxImageBytes
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, fmt.Errorf("adapter: open %q: %w", rel, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("adapter: read %q: %w", rel, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("adapter: %q exceeds %d bytes", rel, maxBytes)
	}
	return data, nil
}
