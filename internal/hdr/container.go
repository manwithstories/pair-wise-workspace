package hdr

import (
	"errors"
	"fmt"

	"hdrcheck/internal/garc"
)

// TLV 标签（见 docs/garc-format.md 第 3 节）。
const (
	TagMasteringDisplay uint16 = 0x0001
	TagContentLight     uint16 = 0x0002
	TagUnitDimension    uint16 = 0x0003
)

const (
	masteringPayloadLen = 36
	cllPayloadLen       = 4
	tlvHeaderLen        = 8 // tag u16 + length u16 + ... + crc u32
)

// Severity 分级：error=结构/数据损坏类，warning=业务不一致类。
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Issue 是解析与校验阶段统一的可审计问题项，report 层原样渲染。
type Issue struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	TrackID  uint32   `json:"track_id,omitempty"`
	Offset   int64    `json:"offset"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`
}

// ContainerMetadata 是元数据区的解析结果。指针为 nil 表示该 TLV 整体缺失。
type ContainerMetadata struct {
	Region    garc.Region        `json:"-"`
	Mastering *MasteringDisplay  `json:"mastering_display,omitempty"`
	CLL       *ContentLightLevel `json:"cll,omitempty"`
	Issues    []Issue            `json:"issues"`
	// Fatal 表示容器区发生不可继续的结构损坏（截断/越界），退出码按 3 处理。
	Fatal bool `json:"-"`
}

// ParseContainer 解析 GARC 元数据区。区长度为 0 时返回全缺省结果。
// 单条记录损坏只记录 Issue 并跳过；只有游标级截断才置 Fatal。
func ParseContainer(f *garc.File) (*ContainerMetadata, error) {
	out := &ContainerMetadata{Region: f.Header.Metadata}
	rg := f.Header.Metadata
	if rg.Length == 0 {
		return out, nil
	}
	r := f.Reader
	cursor := int64(rg.Offset)
	regionEnd, ok := rg.End()
	if !ok {
		return nil, garc.NewError("INTEGER_OVERFLOW", int64(rg.Offset),
			"metadata region offset+length overflows uint32")
	}
	end := int64(regionEnd)
	seen := map[uint16]bool{}

	for cursor < end {
		if end-cursor < tlvHeaderLen {
			out.Fatal = true
			out.Issues = append(out.Issues, truncIssue(cursor, "TLV header"))
			return out, nil
		}
		tag, _ := r.U16(cursor)
		length, _ := r.U16(cursor + 2)
		recEnd := cursor + tlvHeaderLen + int64(length)
		if recEnd > end {
			out.Fatal = true
			out.Issues = append(out.Issues, truncIssue(cursor,
				fmt.Sprintf("tag 0x%04x declares %d payload bytes", tag, length)))
			return out, nil
		}
		wantCRC, _ := r.U32(cursor + 4 + int64(length))
		if err := r.CheckCRC(cursor, 4+int64(length), wantCRC); err != nil {
			var ge *garc.Error
			if errors.As(err, &ge) {
				out.Issues = append(out.Issues, Issue{
					Code: ge.Code, Severity: SeverityError, Offset: ge.Offset,
					Message: ge.Message, Hint: ge.Hint,
				})
			}
			cursor = recEnd
			continue
		}
		payloadOff := cursor + 4
		switch tag {
		case TagMasteringDisplay:
			if seen[tag] {
				out.dupIssue(tag, cursor)
				break
			}
			seen[tag] = true
			if length != masteringPayloadLen {
				out.badDimension(tag, cursor, length, masteringPayloadLen)
				break
			}
			out.Mastering = decodeMastering(r, payloadOff, out)
		case TagContentLight:
			if seen[tag] {
				out.dupIssue(tag, cursor)
				break
			}
			seen[tag] = true
			if length != cllPayloadLen {
				out.badDimension(tag, cursor, length, cllPayloadLen)
				break
			}
			out.CLL = decodeCLL(r, payloadOff, out)
		case TagUnitDimension:
			dim, _ := r.U8(payloadOff)
			if length != 1 || dim != 1 {
				out.Issues = append(out.Issues, Issue{
					Code: "ILLEGAL_DIMENSION", Severity: SeverityError, Offset: cursor,
					Message: fmt.Sprintf("unit dimension byte=%d (want 1)", dim),
					Hint:    "re-export with 1e-4 chromaticity / nit units",
				})
			}
		default:
			out.Issues = append(out.Issues, Issue{
				Code: "UNKNOWN_FIELD", Severity: SeverityWarning, Offset: cursor,
				Message: fmt.Sprintf("unknown TLV tag 0x%04x (%d bytes) skipped", tag, length),
				Hint:    "upgrade hdrcheck if this tag is expected",
			})
		}
		cursor = recEnd
	}
	return out, nil
}

func decodeMastering(r *garc.Reader, off int64, out *ContainerMetadata) *MasteringDisplay {
	md := &MasteringDisplay{
		Present:    true,
		Source:     SourceContainer,
		Provenance: Provenance{Offset: off},
	}
	// 每原色占 2 个 u16（x,y），单条记录 6 原色共 24 字节。
	for i := 0; i < 6; i++ {
		md.DisplayPrimaries[i][0], _ = r.U16(off + int64(i*4))
		md.DisplayPrimaries[i][1], _ = r.U16(off + int64(i*4+2))
	}
	md.WhitePoint[0], _ = r.U16(off + 24)
	md.WhitePoint[1], _ = r.U16(off + 26)
	md.MinDisplayMasteringLuminance, _ = r.U32(off + 28)
	md.MaxDisplayMasteringLuminance, _ = r.U32(off + 32)
	for i, p := range md.DisplayPrimaries {
		for j, v := range p {
			if !ValidateChromaticity(v) {
				out.rangeIssue(off+int64(i*4+j*2), fmt.Sprintf(
					"display primary[%d][%d]=%d outside [0,%d]", i, j, v, MaxChromaticity))
			}
		}
	}
	for j, v := range md.WhitePoint {
		if !ValidateChromaticity(v) {
			out.rangeIssue(off+24+int64(j*2), fmt.Sprintf(
				"white_point[%d]=%d outside [0,%d]", j, v, MaxChromaticity))
		}
	}
	if !ValidateMasteringLuminance(md.MinDisplayMasteringLuminance) {
		out.rangeIssue(off+28, fmt.Sprintf("min mastering luminance %d exceeds %d",
			md.MinDisplayMasteringLuminance, MaxMasteringLum))
	}
	if !ValidateMasteringLuminance(md.MaxDisplayMasteringLuminance) {
		out.rangeIssue(off+32, fmt.Sprintf("max mastering luminance %d exceeds %d",
			md.MaxDisplayMasteringLuminance, MaxMasteringLum))
	}
	if md.MaxDisplayMasteringLuminance < md.MinDisplayMasteringLuminance {
		out.rangeIssue(off+32, fmt.Sprintf(
			"max mastering luminance %d < min %d",
			md.MaxDisplayMasteringLuminance, md.MinDisplayMasteringLuminance))
	}
	return md
}

func decodeCLL(r *garc.Reader, off int64, out *ContainerMetadata) *ContentLightLevel {
	cll, _ := r.U16(off)
	fall, _ := r.U16(off + 2)
	c := &ContentLightLevel{
		MaxContentLightLevel: LightLevel{
			Present: true, Nits: cll, Source: SourceContainer,
			Provenance: Provenance{Offset: off},
		},
		MaxFrameAverageLightLevel: LightLevel{
			Present: true, Nits: fall, Source: SourceContainer,
			Provenance: Provenance{Offset: off + 2},
		},
	}
	if !ValidateCLL(cll) {
		out.rangeIssue(off, fmt.Sprintf("MaxCLL=%d nits outside [%d,%d]", cll, MinCLL, MaxCLL))
	}
	if !ValidateCLL(fall) {
		out.rangeIssue(off+2, fmt.Sprintf("MaxFALL=%d nits outside [%d,%d]", fall, MinCLL, MaxCLL))
	}
	return c
}

func (out *ContainerMetadata) dupIssue(tag uint16, off int64) {
	out.Issues = append(out.Issues, Issue{
		Code: "DUPLICATE_FIELD", Severity: SeverityError, Offset: off,
		Message: fmt.Sprintf("TLV tag 0x%04x appears more than once; first value kept", tag),
		Hint:    "re-export; each container field may appear exactly once",
	})
}

func (out *ContainerMetadata) badDimension(tag uint16, off int64, got, want uint16) {
	out.Issues = append(out.Issues, Issue{
		Code: "ILLEGAL_DIMENSION", Severity: SeverityError, Offset: off,
		Message: fmt.Sprintf("tag 0x%04x payload length=%d, want %d", tag, got, want),
		Hint:    "re-export with the specified fixed payload layout",
	})
}

func (out *ContainerMetadata) rangeIssue(off int64, msg string) {
	out.Issues = append(out.Issues, Issue{
		Code: "VALUE_OUT_OF_RANGE", Severity: SeverityError, Offset: off,
		Message: msg, Hint: "correct the value at source and re-export",
	})
}

func truncIssue(off int64, what string) Issue {
	return Issue{
		Code: "TRUNCATED_RECORD", Severity: SeverityError, Offset: off,
		Message: fmt.Sprintf("truncated inside %s before region end", what),
		Hint:    "the container is cut short; reject and request re-export",
	}
}
