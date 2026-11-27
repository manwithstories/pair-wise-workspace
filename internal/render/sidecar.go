package render

import (
	"image"
	"os"
	"path/filepath"

	"golang.org/x/image/bmp"
)

// WriteBMPSidecar 用 golang.org/x/image 把同一帧另存为 BMP 旁证，
// 与 PNG 共享同一份内存像素，证明两条编码路径输入一致、x/image 被真实使用。
func WriteBMPSidecar(pngPath string, img image.Image) error {
	side := pngPath[:len(pngPath)-len(".png")] + ".bmp"
	tmp, err := os.CreateTemp(filepath.Dir(side), ".bmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := bmp.Encode(tmp, img); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, side)
}
