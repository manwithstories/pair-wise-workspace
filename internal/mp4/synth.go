package mp4

import (
	"bytes"
	"encoding/binary"
	"math/rand"
)

// Fault 枚举夹具生成时注入的故障类型。
type Fault int

const (
	FaultNone Fault = iota
	// FaultTruncate 截断尾部：文件在 mdat 末尾前结束。
	FaultTruncate
	// FaultCorrupt 损坏样本盒：把 stsz 的样本计数写成与 stts 不一致。
	FaultCorrupt
	// FaultIndexMissing 移除 stss 盒，模拟关键帧索引缺页。
	FaultIndexMissing
	// FaultNegativePTS 注入 elst 负 MediaTime，使样本出现负时间戳。
	FaultNegativePTS
)

// SynthSpec 描述一条待生成的轨道。
type SynthSpec struct {
	Handler    string // vide / soun
	Timescale  uint32
	Count      uint32 // 样本数
	Delta      uint32 // 每样本时长（tick）
	KeyEvery   uint32 // 每隔多少样本一个关键帧；0 表示不写 stss
	Seed       int64
	MediaStart int64 // elst MediaTime，负值制造负时间戳
}

type placement struct{ off, size uint32 }

type trackBuild struct {
	spec   SynthSpec
	sizes  []uint32
	data   []byte
	keys   []uint32
	allocs []placement
}

// SynthFile 是确定性 MP4 夹具生成器，不依赖任何外部媒体工具。
// 采用 moov 前置布局：尾部截断会让 mdat 声明越过 EOF，天然可检测。
// 返回完整文件字节与 mdat payload（供测试直接比对样本数据）。
func SynthFile(specs []SynthSpec, fault Fault) ([]byte, []byte, error) {
	builds := make([]*trackBuild, len(specs))
	for i, sp := range specs {
		rng := rand.New(rand.NewSource(sp.Seed))
		sizes := make([]uint32, sp.Count)
		var data bytes.Buffer
		for j := range sizes {
			sizes[j] = uint32(32 + rng.Intn(96))
			chunk := make([]byte, sizes[j])
			if _, err := rng.Read(chunk); err != nil {
				return nil, nil, err
			}
			data.Write(chunk)
		}
		var keys []uint32
		if sp.KeyEvery > 0 {
			for k := uint32(1); k <= sp.Count; k += sp.KeyEvery {
				keys = append(keys, k)
			}
		}
		builds[i] = &trackBuild{spec: sp, sizes: sizes, data: data.Bytes(), keys: keys}
	}

	var mdat bytes.Buffer
	for _, b := range builds {
		var base uint32
		b.allocs = make([]placement, len(b.sizes))
		for j, sz := range b.sizes {
			b.allocs[j] = placement{base, sz}
			base += sz
		}
		mdat.Write(b.data)
	}

	// 第一遍：用占位 mdat 基址 0 组装 moov，测量其长度。
	probe := buildMoov(builds, fault, 0)
	ftyp := box("ftyp", append(
		append(fourccBytes("isom"), u32(0x200)...),
		fourccBytes("isom")...))
	mdatBase := uint32(len(ftyp) + len(probe) + 8)
	// 第二遍：用真实 mdat payload 基址生成最终 moov。
	moov := buildMoov(builds, fault, mdatBase)

	var out bytes.Buffer
	out.Write(ftyp)
	out.Write(moov)
	mdatBox := box("mdat", mdat.Bytes())
	out.Write(mdatBox)
	raw := out.Bytes()

	if fault == FaultTruncate {
		// 真实尾部截断：mdat 头与声明长度保持不变，仅裁掉 payload 尾部
		// 40 字节。声明范围越过 EOF，解析器据此识别并恢复前面的 moov。
		cut := 40
		if cut >= mdat.Len() {
			cut = mdat.Len() / 2
		}
		raw = raw[:len(raw)-cut]
	}
	return raw, mdat.Bytes(), nil
}

func buildMoov(builds []*trackBuild, fault Fault, mdatBase uint32) []byte {
	var traks []byte
	running := mdatBase
	for ti, b := range builds {
		tf := fault
		if ti > 0 {
			tf = FaultNone // 损坏只注入第一轨道，验证故障不跨轨扩散
		}
		traks = append(traks, box("trak", buildTrak(b, running, tf))...)
		running += uint32(len(b.data))
	}
	unknown := box("zzzz", []byte("custom-payload-1234567890"))
	return box("moov", append(traks, unknown...))
}

func box(typ string, payload []byte) []byte {
	h := make([]byte, 8)
	binary.BigEndian.PutUint32(h, uint32(8+len(payload)))
	copy(h[4:], typ)
	return append(h, payload...)
}

func fullBox(typ string, version uint8, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(head, payload...))
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func i32(v int32) []byte { return u32(uint32(v)) }

func fourccBytes(s string) []byte {
	b := []byte(s)
	if len(b) != 4 {
		b = make([]byte, 4)
		copy(b, s)
	}
	return b
}
