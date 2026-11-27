package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"archivemedia/internal/media"
)

// sha256File 计算文件内容的 SHA-256 十六进制摘要。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", media.E(media.KindCheckpoint, "pipeline.sha256",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"打开输出文件失败", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", media.E(media.KindCheckpoint, "pipeline.sha256",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"读取输出文件失败", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// baseName 返回路径的文件名部分。
func baseName(p string) string { return filepath.Base(p) }
