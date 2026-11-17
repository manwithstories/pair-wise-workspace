// Package app 串联 GARC reader → container/SEI 解析 → 统一模型 →
// consistency validator → reporter，并集中裁决退出码。
package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"hdrcheck/internal/garc"
	"hdrcheck/internal/hdr"
	"hdrcheck/internal/validate"
)

// 退出码契约（见 docs/garc-format.md 第 7 节）。
const (
	ExitOK            = 0
	ExitBusiness      = 2 // 业务冲突/缺省（严格模式）
	ExitCorruptOrArgs = 3 // 容器损坏或参数错误
	ExitInternal      = 4 // 内部异常
)

// PipelineResult 是一次完整解析校验的产物。
type PipelineResult struct {
	File      *garc.File
	Container *hdr.ContainerMetadata
	SEI       *hdr.SEIResult
	Report    *validate.Report
	SHA256    string
	InputName string
}

// Run 读取并解析单个 GARC 文件。返回的 error 已按退出码分类：
// 参数/损坏类为 *garc.Error（退出 3），其余为内部错误（退出 4）。
func Run(path string) (*PipelineResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, garc.NewError("INPUT_READ_ERROR", -1,
			fmt.Sprintf("cannot read %s: %v", path, err))
	}
	sum := sha256.Sum256(data)
	f, err := garc.Parse(data)
	if err != nil {
		return nil, err // 均为 *garc.Error，退出码 3
	}
	container, err := hdr.ParseContainer(f)
	if err != nil {
		return nil, err
	}
	sei, err := hdr.ParseSEI(f)
	if err != nil {
		return nil, err
	}
	rep := validate.Build(f, container, sei, hex.EncodeToString(sum[:]))
	return &PipelineResult{
		File:      f,
		Container: container,
		SEI:       sei,
		Report:    rep,
		SHA256:    hex.EncodeToString(sum[:]),
		InputName: path,
	}, nil
}

// DecideExitCode 按模式裁决退出码。
//   - 结构损坏/Fatal/SeverityError：3（两种模式一致）
//   - 仅 warning：strict 下 2，审计模式 0
func DecideExitCode(rep *validate.Report, strict bool) int {
	if rep.Fatal || rep.Summary.ErrorCount > 0 {
		return ExitCorruptOrArgs
	}
	if strict && (rep.Summary.ConflictCount > 0 || rep.Summary.MissingCount > 0 ||
		rep.Summary.WarningCount > 0) {
		return ExitBusiness
	}
	return ExitOK
}

// IsCorruptError 判断错误是否属于容器/参数类（退出码 3）。
func IsCorruptError(err error) bool {
	var ge *garc.Error
	return errors.As(err, &ge)
}
