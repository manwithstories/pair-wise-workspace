package hdr_test

import (
	"testing"

	"hdrcheck/internal/fixture"
	"hdrcheck/internal/garc"
	"hdrcheck/internal/hdr"
)

// 用构建器手工拼装边缘场景，覆盖生成器未直接产出的安全路径。
func parseBuilder(t *testing.T, b *fixture.Builder) *garc.File {
	t.Helper()
	data, err := b.Build()
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	f, err := garc.Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

func TestUnknownTLVTagSkippedNotFatal(t *testing.T) {
	b, _ := fixture.NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddUnitDimensionTLV(1)
	b.AddCLLTLV(1000, 400)
	b.AddRawTLV(0x7777, []byte{1, 2, 3}) // 未知 tag，应告警但续读
	f := parseBuilder(t, b)
	c, err := hdr.ParseContainer(f)
	if err != nil || c.Fatal {
		t.Fatalf("unknown tag must not be fatal: err=%v fatal=%v", err, c.Fatal)
	}
	if !hasCode(c.Issues, "UNKNOWN_FIELD") {
		t.Errorf("want UNKNOWN_FIELD, got %+v", c.Issues)
	}
	if c.CLL == nil {
		t.Error("known fields before unknown tag must still parse")
	}
}

func TestIllegalDimensionByte(t *testing.T) {
	b, _ := fixture.NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddUnitDimensionTLV(9) // 非法量纲
	b.AddCLLTLV(1000, 400)
	f := parseBuilder(t, b)
	c, _ := hdr.ParseContainer(f)
	if !hasCode(c.Issues, "ILLEGAL_DIMENSION") {
		t.Errorf("want ILLEGAL_DIMENSION, got %+v", c.Issues)
	}
}

func TestOrphanSEIRecord(t *testing.T) {
	b, _ := fixture.NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddCLLTLV(1000, 400)
	b.AddVideoTrack(1, true, 0)
	// track 99 不存在于轨道表。
	b.AddSEICLL(99, 0, 0, 1000, 1000, 400)
	f := parseBuilder(t, b)
	sei, err := hdr.ParseSEI(f)
	if err != nil {
		t.Fatalf("ParseSEI: %v", err)
	}
	if !hasCode(sei.Issues, "ORPHAN_SEI_RECORD") {
		t.Errorf("want ORPHAN_SEI_RECORD, got %+v", sei.Issues)
	}
	if len(sei.Records) != 0 {
		t.Error("orphan record must not be retained")
	}
}

func TestIllegalTimeRangeRejected(t *testing.T) {
	b, _ := fixture.NewBuilder(garc.EndianLittle)
	b.AddVideoTrack(1, true, 0)
	// time_end == time_start，非法（构建器允许任意值，由解析器裁决）。
	b.AddSEICLL(1, 0, 500, 500, 1000, 400)
	f := parseBuilder(t, b)
	sei, _ := hdr.ParseSEI(f)
	if !hasCode(sei.Issues, "ILLEGAL_TIME_RANGE") {
		t.Errorf("want ILLEGAL_TIME_RANGE, got %+v", sei.Issues)
	}
}

func TestOutOfRangeLightLevel(t *testing.T) {
	// MaxCLL=0 在容器侧即判非法。
	b, _ := fixture.NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddUnitDimensionTLV(1)
	b.AddCLLTLV(0, 400)
	b.AddVideoTrack(1, true, 0)
	f := parseBuilder(t, b)
	c, _ := hdr.ParseContainer(f)
	if !hasCode(c.Issues, "VALUE_OUT_OF_RANGE") {
		t.Errorf("zero MaxCLL must be VALUE_OUT_OF_RANGE, got %+v", c.Issues)
	}
}

func TestFileSizeLimitEnforced(t *testing.T) {
	// 仅触及上限判断，不实际分配 512MiB：直接构造略大的 Reader 入参。
	big := make([]byte, garc.MaxFileSize+1)
	if _, err := garc.NewReader(big); err == nil {
		t.Fatal("file exceeding 512 MiB must be rejected")
	}
}
