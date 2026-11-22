package adapter

import (
	"os"
	"path/filepath"
	"strings"

	"mediacheck/internal/model"
)

// 安全上限默认值：防止畸形 box 触发超大分配，并约束输入总量。
const (
	DefaultMaxInputBytes int64 = 512 << 20 // 512 MiB
	DefaultMaxBoxSize    int64 = 64 << 20  // 64 MiB
)

// CheckInputLimits 在任何解析前校验输入总量与单片大小。
func CheckInputLimits(in ParseInput) error {
	maxInput := in.MaxInputBytes
	if maxInput <= 0 {
		maxInput = DefaultMaxInputBytes
	}
	maxBox := in.MaxBoxSize
	if maxBox <= 0 {
		maxBox = DefaultMaxBoxSize
	}
	var total int64
	total += int64(len(in.InitSegment))
	for i, f := range in.Fragments {
		n := int64(len(f))
		if n > maxBox {
			return &model.MediaError{
				Category: model.ErrOversizeInput,
				Message:  "fragment exceeds box limit",
				Offset:   int64(i),
			}
		}
		total += n
	}
	if total > maxInput {
		return &model.MediaError{
			Category: model.ErrOversizeInput,
			Message:  "total input exceeds configured limit",
			Offset:   total,
		}
	}
	return nil
}

// SafeOutputPath 校验输出路径：必须是绝对/规范化后位于 baseDir 之内，
// 禁止路径逃逸与覆盖输入目录；返回清理后的绝对路径。
func SafeOutputPath(baseDir, name string) (string, error) {
	if baseDir == "" || name == "" {
		return "", &model.MediaError{Category: model.ErrPathViolation, Message: "empty path"}
	}
	absBase, err := filepath.Abs(filepath.Clean(baseDir))
	if err != nil {
		return "", &model.MediaError{Category: model.ErrPathViolation, Message: err.Error()}
	}
	if filepath.IsAbs(name) || strings.Contains(name, "..") {
		return "", &model.MediaError{Category: model.ErrPathViolation, Message: "name must be a relative base name: " + name}
	}
	full := filepath.Join(absBase, filepath.Clean(name))
	if !strings.HasPrefix(full, absBase+string(os.PathSeparator)) && full != absBase {
		return "", &model.MediaError{Category: model.ErrPathViolation, Message: "path escapes output dir: " + full}
	}
	return full, nil
}
