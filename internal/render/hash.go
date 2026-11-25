package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// BytesSHA256 返回字节切片的 SHA256。
func BytesSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FileSHA256 流式计算文件 SHA256，避免把大文件整体读入内存。
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SequenceHash 按稳定帧序号聚合每帧 PNG 哈希。
// 任何一帧像素变化或帧顺序变化都会改变此哈希。
func SequenceHash(frameHashes []string) string {
	h := sha256.New()
	for i, fh := range frameHashes {
		fmt.Fprintf(h, "%06d  %s\n", i, fh)
	}
	return hex.EncodeToString(h.Sum(nil))
}
