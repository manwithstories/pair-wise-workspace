// Package sidecar 用 ISO BMFF 盒结构承载合成素材的确定性元数据。
// 文件由 ftyp + free 两个顶层盒组成：free 盒内放 JSON 帧时间戳表，
// 裸码流本身与 sidecar 分离，扫描器只读码流、时间轴只读 sidecar。
package sidecar

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// FrameMeta 是单帧的时间戳与随机访问标记。RawTS 为 32 位回绕时间戳，
// 单位由 Meta.Timescale 定义。
type FrameMeta struct {
	RawTS    uint32 `json:"raw_ts"`
	Keyframe bool   `json:"keyframe"`
}

// Meta 是 sidecar 文件的完整元数据负载。
type Meta struct {
	Codec        string      `json:"codec"`
	TrackID      int         `json:"track_id"`
	Timescale    uint32      `json:"timescale"`     // 每秒滴答数（通常 90000）
	NominalStep  uint32      `json:"nominal_step"`  // 恒定帧率每帧滴答
	KeyframeEvery int        `json:"keyframe_every"`
	Frames       []FrameMeta `json:"frames"`
	StreamSize   int64       `json:"stream_size"`
	StreamSHA256 string      `json:"stream_sha256"`
}

// 自定义顶层盒类型，避免与标准盒冲突。
func boxTypeMeta() mp4.BoxType { return mp4.StrToBoxType("maud") }

func init() {
	// 注册自定义盒，使 ReadBoxStructure 能把它解析为 AnyTypeBox。
	mp4.AddAnyTypeBoxDef(&mp4.AnyTypeBox{}, boxTypeMeta())
}

// Write 把元数据以 ftyp + maud 两盒写入 w。
func Write(w io.WriteSeeker, m *Meta) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	writer := mp4.NewWriter(w)

	ftypBI, err := writer.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()})
	if err != nil {
		return err
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:     [4]byte{'m', 'a', 'u', 'd'},
		MinorVersion:   1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: mp4.BrandISO8()},
		},
	}
	if _, err := mp4.Marshal(writer, ftyp, mp4.Context{}); err != nil {
		return err
	}
	if _, err := writer.EndBox(); err != nil {
		return err
	}
	_ = ftypBI

	metaBI, err := writer.StartBox(&mp4.BoxInfo{Type: boxTypeMeta()})
	if err != nil {
		return err
	}
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	if _, err := writer.EndBox(); err != nil {
		return err
	}
	_ = metaBI
	return nil
}

// Read 从 r 解析并返回 maud 盒内的元数据；ftyp 仅做结构校验。
func Read(r io.ReadSeeker) (*Meta, error) {
	var (
		sawFtyp bool
		meta    *Meta
	)
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeFtyp():
			sawFtyp = true
		case boxTypeMeta():
			var payload bytes.Buffer
			if _, err := h.ReadData(&payload); err != nil {
				return nil, err
			}
			var m Meta
			if err := json.Unmarshal(payload.Bytes(), &m); err != nil {
				return nil, fmt.Errorf("maud 盒 JSON 解析失败: %w", err)
			}
			meta = &m
		}
		return nil, nil // 不需要嵌套结构
	}
	if _, err := mp4.ReadBoxStructure(r, handler); err != nil && err != io.EOF {
		return nil, err
	}
	if !sawFtyp {
		return nil, fmt.Errorf("sidecar 缺少 ftyp 盒")
	}
	if meta == nil {
		return nil, fmt.Errorf("sidecar 缺少 maud 元数据盒")
	}
	return meta, nil
}

// Marshal 是便于测试的内存封装。
func Marshal(m *Meta) ([]byte, error) {
	f := newMemFile()
	if err := Write(f, m); err != nil {
		return nil, err
	}
	return f.bytes(), nil
}
