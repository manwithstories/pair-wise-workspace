// Package manifest 解析本地 HLS 媒体清单与 DASH MPD 的 SegmentTimeline，
// 把片段描述以统一 Fragment 形式交给 segment 读取器。
//
// 安全边界：清单最大 10 MiB、单清单最多 MaxFragments 个片段、路径不得越界
// （拒绝绝对路径、盘符与任何形式的 ".." 上升段），重复序号内容冲突时整单拒绝。
package manifest

import (
	"errors"
	"strings"
)

// 解析硬性限制（约束条款）。
const (
	// MaxManifestBytes 清单文件最大 10 MiB。
	MaxManifestBytes = 10 << 20
	// MaxFragments 单清单允许的最大片段数。
	MaxFragments = 10_000
	// MaxURLLength 单个片段路径的最大长度。
	MaxURLLength = 4_096
)

// 协议级错误类别，均带片段/序号定位信息。
var (
	// ErrManifestTooLarge 清单超过 10 MiB。
	ErrManifestTooLarge = errors.New("manifest: exceeds 10 MiB size limit")
	// ErrTooManyFragments 片段数超过上限。
	ErrTooManyFragments = errors.New("manifest: fragment count exceeds limit")
	// ErrDuplicateSequence 重复序号且内容冲突。
	ErrDuplicateSequence = errors.New("manifest: duplicate sequence with conflicting content")
	// ErrPathOutOfScope 片段路径越界（绝对路径或包含 ".."）。
	ErrPathOutOfScope = errors.New("manifest: segment path out of scope")
	// ErrInvalidManifest 清单语法/结构无效。
	ErrInvalidManifest = errors.New("manifest: invalid syntax")
	// ErrUnsupportedProtocol 不是受支持的清单类型。
	ErrUnsupportedProtocol = errors.New("manifest: unsupported protocol")
)

// Protocol 标识清单协议。
type Protocol string

// 支持的协议常量。
const (
	ProtoHLS  Protocol = "hls"
	ProtoDASH Protocol = "dash"
)

// Fragment 是与协议无关的片段描述，读取器只依赖本结构。
type Fragment struct {
	Sequence      int    `json:"sequence"`       // 清单内单调序号（从 0 开始）
	URI           string `json:"uri"`            // 已校验的相对路径
	DurationTicks int64  `json:"duration_ticks"` // 标称时长（90 kHz Tick）
	Discontinuity bool   `json:"discontinuity"`  // HLS #EXT-X-DISCONTINUITY 边界
	StartTicks    int64  `json:"start_ticks"`    // 清单时间轴上的标称起点
}

// Manifest 是解析后的统一清单视图。
type Manifest struct {
	Protocol       Protocol   `json:"protocol"`
	TargetDuration int64      `json:"target_duration_ticks"`
	Fragments      []Fragment `json:"fragments"`
	MediaSequence  int        `json:"media_sequence"` // HLS 起始媒体序号
}

// FragmentBySequence 按序号返回片段，找不到时返回 false。
func (m *Manifest) FragmentBySequence(seq int) (Fragment, bool) {
	for _, f := range m.Fragments {
		if f.Sequence == seq {
			return f, true
		}
	}
	return Fragment{}, false
}

// validatePath 保证片段路径始终位于清单目录作用域内：
// 拒绝绝对路径、Windows 盘符、UNC 路径以及任何 ".." 路径段。
func validatePath(p string) error {
	if p == "" || len(p) > MaxURLLength {
		return ErrPathOutOfScope
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return ErrPathOutOfScope
	}
	// 带 scheme 的 URI（http://、file:// 等）一律拒绝，本工具仅处理本地相对路径。
	if i := strings.Index(p, "://"); i > 0 {
		return ErrPathOutOfScope
	}
	// Windows 盘符（C:\ 或 C:/）。
	if len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == '\\') {
		return ErrPathOutOfScope
	}
	// 反斜杠归一化为斜杠后逐段检查。
	normalized := strings.ReplaceAll(p, "\\", "/")
	for _, seg := range strings.Split(normalized, "/") {
		if seg == ".." {
			return ErrPathOutOfScope
		}
	}
	return nil
}

func checkFragmentCount(n int) error {
	if n > MaxFragments {
		return ErrTooManyFragments
	}
	return nil
}
