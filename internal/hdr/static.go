// Package hdr 定义 HDR 静态元数据的统一模型，并实现容器 TLV 与 HDR-SEI
// 两路解析。解析器只负责“忠实读出 + 标注来源”，合并与冲突分级在
// internal/validate 中完成。
package hdr

// Source 标识单个逻辑字段的实际来源。
type Source string

const (
	// SourceContainer 来自 GARC 元数据区 TLV。
	SourceContainer Source = "container"
	// SourceSEI 来自视频轨 HDR-SEI 补充增强信息。
	SourceSEI Source = "sei"
	// SourceDefault 两路均缺省时按规范补的默认值（本工具不静默写零，
	// 默认仅用于显式标注的缺省补全场景）。
	SourceDefault Source = "default"
	// SourceMissing 两路均无该字段。
	SourceMissing Source = "missing"
)

// Priority 来源优先级：container > sei > default > missing。
// 相等性裁决以 container 为准，但冲突必须同时保留两个值。
func Priority(s Source) int {
	switch s {
	case SourceContainer:
		return 4
	case SourceSEI:
		return 3
	case SourceDefault:
		return 2
	case SourceMissing:
		return 1
	default:
		return 0
	}
}

// 量纲与范围常量（见 docs/garc-format.md 第 6 节）。
const (
	// 色坐标单位 0.0001，合法区间 [0, 50000]。
	MaxChromaticity = 50000
	// mastering 亮度单位 0.0001 nit，上限 100,000,000（=10000 nit）。
	MaxMasteringLum = 100_000_000
	// MaxCLL / MaxFALL 单位 nit，合法区间 [1, 10000]；0 非法。
	MinCLL = 1
	MaxCLL = 10000
)

// Provenance 是任何被读出字段的可审计出处。
type Provenance struct {
	Offset    int64  `json:"offset"`
	TrackID   uint32 `json:"track_id,omitempty"`
	SeqNum    uint32 `json:"seq_num,omitempty"`
	TimeStart uint32 `json:"time_start_ms,omitempty"`
	TimeEnd   uint32 `json:"time_end_ms,omitempty"`
}

// MasteringDisplay 是母版显示信息（SMPTE ST 2086 等价集合）。
// 坐标单位 0.0001，亮度单位 0.0001 nit。
type MasteringDisplay struct {
	Present          bool         `json:"present"`
	Source           Source       `json:"source"`
	DisplayPrimaries [6][2]uint16 `json:"display_primaries"`
	WhitePoint       [2]uint16    `json:"white_point"`
	// MinDisplayMasteringLuminance / MaxDisplayMasteringLuminance 单位 1e-4 nit。
	MinDisplayMasteringLuminance uint32     `json:"min_display_mastering_luminance"`
	MaxDisplayMasteringLuminance uint32     `json:"max_display_mastering_luminance"`
	Provenance                   Provenance `json:"provenance"`
}

// LightLevel 是单个亮度量级字段（MaxCLL 或 MaxFALL），单位 nit。
// 缺失时 Present=false、Source=missing，绝不静默写零。
type LightLevel struct {
	Present    bool       `json:"present"`
	Nits       uint16     `json:"nits,omitempty"`
	Source     Source     `json:"source"`
	Provenance Provenance `json:"provenance"`
}

// ContentLightLevel 是 MaxCLL + MaxFALL 组合（CTA-861 等价集合）。
type ContentLightLevel struct {
	MaxContentLightLevel      LightLevel `json:"max_content_light_level"`
	MaxFrameAverageLightLevel LightLevel `json:"max_frame_average_light_level"`
}

// MissingMastering 返回标注为 missing 的空母版显示信息。
func MissingMastering(off int64) MasteringDisplay {
	return MasteringDisplay{Present: false, Source: SourceMissing, Provenance: Provenance{Offset: off}}
}

// MissingLightLevel 返回标注为 missing 的空亮度字段。
func MissingLightLevel(off int64) LightLevel {
	return LightLevel{Present: false, Source: SourceMissing, Provenance: Provenance{Offset: off}}
}

// ValidateChromaticity 校验单个 0.0001 单位色坐标。
func ValidateChromaticity(v uint16) bool { return v <= MaxChromaticity }

// ValidateMasteringLuminance 校验 1e-4 nit 单位的母版亮度。
func ValidateMasteringLuminance(v uint32) bool { return v <= MaxMasteringLum }

// ValidateCLL 校验 nit 单位的内容亮度；0 视为非法。
func ValidateCLL(v uint16) bool { return v >= MinCLL && v <= MaxCLL }

// EqualMastering 比较两套母版显示信息的数值部分（忽略来源与出处）。
func EqualMastering(a, b MasteringDisplay) bool {
	if a.MinDisplayMasteringLuminance != b.MinDisplayMasteringLuminance ||
		a.MaxDisplayMasteringLuminance != b.MaxDisplayMasteringLuminance ||
		a.WhitePoint != b.WhitePoint {
		return false
	}
	for i := range a.DisplayPrimaries {
		if a.DisplayPrimaries[i] != b.DisplayPrimaries[i] {
			return false
		}
	}
	return true
}
