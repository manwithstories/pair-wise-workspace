package fixture

import (
	"fmt"
	"os"
	"path/filepath"

	"hdrcheck/internal/garc"
	"hdrcheck/internal/hdr"
)

// 标准 DCI-P3/D65 母版显示（坐标 1e-4；亮度 1e-4 nit：min 0.005 nit、max 1000 nit）。
func canonicalMastering() hdr.MasteringDisplay {
	return hdr.MasteringDisplay{
		DisplayPrimaries: [6][2]uint16{
			{35400, 14600}, {8500, 39850}, {6550, 2300},
			{35400, 14600}, {8500, 39850}, {6550, 2300},
		},
		WhitePoint:                   [2]uint16{15635, 16450},
		MinDisplayMasteringLuminance: 50,
		MaxDisplayMasteringLuminance: 10_000_000,
	}
}

// Scenario 描述一个夹具文件及其构造函数，供命令与测试枚举。
type Scenario struct {
	Name  string
	Build func() (*Builder, error)
}

// Scenarios 返回全部夹具场景（顺序即写入顺序）。
func Scenarios() []Scenario {
	return []Scenario{
		{"hdr-valid.garc", buildValid},
		{"hdr-mixed.garc", buildMixed},
		{"hdr-inherited.garc", buildInherited},
		{"hdr-missing-fields.garc", buildMissing},
		{"hdr-truncated.garc", buildTruncated},
		{"hdr-badchecksum.garc", buildBadChecksum},
		{"hdr-duplicate.garc", buildDuplicate},
		{"hdr-unknown-version.garc", buildUnknownVersion},
		{"hdr-overlap.garc", buildOverlap},
		{"hdr-length-mismatch.garc", buildLengthMismatch},
	}
}

// 1) 全正常：容器 TLV 与三轨 SEI 完全一致，无任何问题，退出码 0。
func buildValid() (*Builder, error) {
	b, _ := NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddUnitDimensionTLV(1)
	b.AddMasteringTLV(canonicalMastering())
	b.AddCLLTLV(1000, 400)
	for _, id := range []uint32{1, 2, 3} {
		b.AddVideoTrack(id, true, 2)
		b.AddSEIMastering(id, 0, 0, 5000, canonicalMastering())
		b.AddSEICLL(id, 1, 5000, 10000, 1000, 400)
	}
	return b, nil
}

//  2. 混合（验收中心件）：
//     轨1 一致；轨2 SEI MaxFALL=250 与容器 400 冲突（MaxCLL 仍为 1000）；
//     轨3 无 HDR 标记且无 SEI（HDR_FLAG_MISSING，计入 missing）。
func buildMixed() (*Builder, error) {
	b, _ := NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddUnitDimensionTLV(1)
	b.AddMasteringTLV(canonicalMastering())
	b.AddCLLTLV(1000, 400)
	b.AddVideoTrack(1, true, 2)
	b.AddSEIMastering(1, 0, 0, 5000, canonicalMastering())
	b.AddSEICLL(1, 1, 5000, 10000, 1000, 400)
	b.AddVideoTrack(2, true, 2)
	b.AddSEIMastering(2, 0, 0, 5000, canonicalMastering())
	b.AddSEICLL(2, 1, 5000, 10000, 1000, 250) // MaxFALL 冲突
	b.AddVideoTrack(3, false, 0)              // 无 HDR 标记
	return b, nil
}

// 3) 容器缺省、SEI 继承：无容器 TLV，全部字段来自 SEI。
func buildInherited() (*Builder, error) {
	b, _ := NewBuilder(garc.EndianBig) // 同时覆盖大端路径
	b.AddVideoTrack(1, true, 2)
	b.AddSEIMastering(1, 0, 0, 5000, canonicalMastering())
	b.AddSEICLL(1, 1, 5000, 10000, 1000, 400)
	return b, nil
}

// 4) 字段缺失：有 HDR 标记但两路均无 CLL，FIELD_MISSING（warning，退出码 2）。
func buildMissing() (*Builder, error) {
	b, _ := NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddUnitDimensionTLV(1)
	b.AddMasteringTLV(canonicalMastering())
	b.AddVideoTrack(1, true, 1)
	b.AddSEIMastering(1, 0, 0, 5000, canonicalMastering())
	return b, nil
}

// 5) 截断：正常文件尾部截断，TRUNCATED/越界类（退出码 3）。
func buildTruncated() (*Builder, error) {
	b, _ := buildValid()
	b.TruncateTail(12)
	return b, nil
}

// 6) 坏校验：翻转第一条 SEI 记录内部字节，CHECKSUM_ERROR（退出码 3）。
func buildBadChecksum() (*Builder, error) {
	b, _ := NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddUnitDimensionTLV(1)
	b.AddMasteringTLV(canonicalMastering())
	b.AddCLLTLV(1000, 400)
	for _, id := range []uint32{1, 2, 3} {
		b.AddVideoTrack(id, true, 2)
		b.AddSEIMastering(id, 0, 0, 5000, canonicalMastering())
		b.AddSEICLL(id, 1, 5000, 10000, 1000, 400)
	}
	// 预构建取得布局偏移；损坏注入不改变布局，最终 Build 偏移一致。
	if _, err := b.Build(); err != nil {
		return nil, err
	}
	b.FlipByte(b.seis[0].offset + 17 + 1)
	return b, nil
}

// 7) 重复字段：mastering TLV 出现两次，DUPLICATE_FIELD（退出码 3）。
func buildDuplicate() (*Builder, error) {
	b, _ := NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddUnitDimensionTLV(1)
	b.AddMasteringTLV(canonicalMastering())
	b.AddMasteringTLV(canonicalMastering()) // 重复
	b.AddCLLTLV(1000, 400)
	b.AddVideoTrack(1, true, 1)
	b.AddSEICLL(1, 0, 0, 5000, 1000, 400)
	return b, nil
}

// 8) 未知版本：version=9，UNKNOWN_VERSION（退出码 3）。
func buildUnknownVersion() (*Builder, error) {
	b, _ := buildValid()
	b.CorruptVersion(9)
	return b, nil
}

// 9) 时间重叠：同轨两条 SEI 时间区间相交，RECORDS_OVERLAP（退出码 3）。
func buildOverlap() (*Builder, error) {
	b, _ := NewBuilder(garc.EndianLittle)
	b.EnableContainerHDR()
	b.AddUnitDimensionTLV(1)
	b.AddMasteringTLV(canonicalMastering())
	b.AddCLLTLV(1000, 400)
	b.AddVideoTrack(1, true, 2)
	b.AddSEICLL(1, 0, 0, 5000, 1000, 400)
	b.AddSEICLL(1, 1, 4000, 9000, 1000, 400) // 与上一条 [0,5000) 相交
	return b, nil
}

// 10) 声明长度不符：file_size 被改大，LENGTH_MISMATCH（退出码 3）。
func buildLengthMismatch() (*Builder, error) {
	b, _ := buildValid()
	b.CorruptDeclaredFileSize(999_999)
	return b, nil
}

// GenerateAll 在 dir 下生成全部夹具，返回写入的文件名列表。
func GenerateAll(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("fixture: create dir: %w", err)
	}
	var names []string
	for _, sc := range Scenarios() {
		b, err := sc.Build()
		if err != nil {
			return nil, fmt.Errorf("fixture %s: %w", sc.Name, err)
		}
		data, err := b.Build()
		if err != nil {
			return nil, fmt.Errorf("fixture %s: %w", sc.Name, err)
		}
		path := filepath.Join(dir, sc.Name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, fmt.Errorf("fixture %s: %w", sc.Name, err)
		}
		names = append(names, sc.Name)
	}
	return names, nil
}
