package sample

import (
	"bytes"

	gomp4 "github.com/abema/go-mp4"
)

// VideoFragment 描述一个视频分片的时间与关键帧信息。
type VideoFragment struct {
	BaseDecodeTime uint64 // 90kHz 刻度
	SampleDuration uint32
	SampleCount    uint32
	Keyframe       bool
}

// memWriteSeeker 支持真实 Seek 的内存 WriteSeeker（go-mp4 回写长度需要）。
type memWriteSeeker struct {
	data []byte
	off  int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	end := m.off + int64(len(p))
	if end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	copy(m.data[m.off:end], p)
	m.off = end
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var t int64
	switch whence {
	case 0:
		t = offset
	case 1:
		t = m.off + offset
	case 2:
		t = int64(len(m.data)) + offset
	}
	m.off = t
	return t, nil
}

func flags24(v uint32) [3]byte { return [3]byte{byte(v >> 16), byte(v >> 8), byte(v)} }

// BuildVideoFMP4 按确定性分片规格生成 fMP4 字节流。
func BuildVideoFMP4(frags []VideoFragment) []byte {
	mem := &memWriteSeeker{}
	w := gomp4.NewWriter(mem)
	start := func(t gomp4.BoxType) {
		if _, e := w.StartBox(&gomp4.BoxInfo{Type: t}); e != nil {
			panic(e)
		}
	}
	end := func() {
		if _, e := w.EndBox(); e != nil {
			panic(e)
		}
	}
	marshal := func(b gomp4.IImmutableBox) {
		if _, e := gomp4.Marshal(w, b, gomp4.Context{}); e != nil {
			panic(e)
		}
	}

	start(gomp4.BoxTypeFtyp())
	marshal(&gomp4.Ftyp{
		MajorBrand:        [4]byte{'i', 's', 'o', 'm'},
		CompatibleBrands:  []gomp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}},
	})
	end()
	start(gomp4.BoxTypeMoov())
	start(gomp4.BoxTypeMvhd())
	marshal(&gomp4.Mvhd{Timescale: VideoRate, DurationV0: 0xFFFFFFFF})
	end()
	start(gomp4.BoxTypeTrak())
	start(gomp4.BoxTypeTkhd())
	marshal(&gomp4.Tkhd{TrackID: 1, DurationV0: 0xFFFFFFFF})
	end()
	start(gomp4.BoxTypeMdia())
	start(gomp4.BoxTypeMdhd())
	marshal(&gomp4.Mdhd{Timescale: VideoRate, DurationV0: 0xFFFFFFFF})
	end()
	start(gomp4.BoxTypeHdlr())
	marshal(&gomp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}})
	end()
	start(gomp4.BoxTypeMinf())
	start(gomp4.BoxTypeStbl())
	start(gomp4.BoxTypeStsd())
	marshal(&gomp4.Stsd{EntryCount: 0})
	end()
	start(gomp4.BoxTypeStts())
	marshal(&gomp4.Stts{EntryCount: 0})
	end()
	start(gomp4.BoxTypeStsc())
	marshal(&gomp4.Stsc{EntryCount: 0})
	end()
	start(gomp4.BoxTypeStsz())
	marshal(&gomp4.Stsz{SampleCount: 0})
	end()
	start(gomp4.BoxTypeStco())
	marshal(&gomp4.Stco{EntryCount: 0})
	end()
	end()
	end()
	start(gomp4.BoxTypeTrex())
	marshal(&gomp4.Trex{TrackID: 1, DefaultSampleDuration: 3000})
	end()
	end()
	end()

	for i, f := range frags {
		start(gomp4.BoxTypeMoof())
		start(gomp4.BoxTypeMfhd())
		marshal(&gomp4.Mfhd{SequenceNumber: uint32(i + 1)})
		end()
		start(gomp4.BoxTypeTraf())
		start(gomp4.BoxTypeTfhd())
		marshal(&gomp4.Tfhd{
			FullBox: gomp4.FullBox{Flags: flags24(gomp4.TfhdDefaultSampleFlagsPresent)},
			TrackID: 1,
		})
		end()
		start(gomp4.BoxTypeTfdt())
		marshal(&gomp4.Tfdt{
			FullBox:               gomp4.FullBox{Version: 1},
			BaseMediaDecodeTimeV1: f.BaseDecodeTime,
		})
		end()
		start(gomp4.BoxTypeTrun())
		entries := make([]gomp4.TrunEntry, f.SampleCount)
		for j := range entries {
			var sf uint32
			if !f.Keyframe {
				sf = 0x00010000 // non-sync
			}
			entries[j] = gomp4.TrunEntry{SampleDuration: f.SampleDuration, SampleFlags: sf}
		}
		marshal(&gomp4.Trun{
			FullBox:     gomp4.FullBox{Flags: flags24(0x100 | 0x400)},
			SampleCount: f.SampleCount,
			Entries:     entries,
		})
		end()
		end()
		end()
		start(gomp4.BoxTypeMdat())
		w.Write(make([]byte, 4*int(f.SampleCount)))
		end()
	}
	return mem.data
}

// DefaultVideoFragments 生成与音频时长匹配、每 2 秒一个关键帧的视频分片。
func DefaultVideoFragments(audioPackets int) []VideoFragment {
	// 音频总时长（90kHz 刻度）：每包 10ms => 900 ticks。
	const perPacketTicks = uint64(VideoRate / 100) // 900
	var frags []VideoFragment
	var t uint64
	keyEvery := 200 // 每 200 个 10ms 分片 ≈ 2s
	for i := 0; i < audioPackets; i++ {
		frags = append(frags, VideoFragment{
			BaseDecodeTime: t,
			SampleDuration: 900,
			SampleCount:    1,
			Keyframe:       i%keyEvery == 0,
		})
		t += perPacketTicks
	}
	return frags
}

// Reader 返回 fMP4 字节的 bytes.Reader，便于检查器 Seek。
func Reader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
