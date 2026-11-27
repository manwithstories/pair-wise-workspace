package render

import (
	"path/filepath"
	"strings"

	"medpipe/internal/clock"
)

// clockSafeJoin 在输出目录内解析帧文件名，拒绝绝对路径与 ".." 越界。
func clockSafeJoin(dir, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", clock.NewError(clock.CodePathRejected, -1, "render: 拒绝绝对路径 %s", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", clock.NewError(clock.CodePathRejected, -1, "render: 路径越界 %s", name)
	}
	return filepath.Join(dir, clean), nil
}
