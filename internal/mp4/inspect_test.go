package mp4

import (
	"bytes"
	"testing"

	gomp4 "github.com/abema/go-mp4"
)

// fragSpec 描述一个测试分片。
type fragSpec struct {
	baseDecodeTime uint64
	// 每个样本：duration 与是否关键帧。
	samples []struct {
		duration uint32
		keyframe bool
	}
}

// buildFMP4 用 go-mp4 Writer 在内存中生成最小 fMP4（ftyp+moov+若干 moof/mdat）。
func buildFMP4(t *testing.T, frags []fragSpec) *bytes.Reader {
	t.Helper()
	mem := &memWriteSeeker{}
	w := gomp4.NewWriter(mem)

	start := func(typ gomp4.BoxType) {
		t.Helper()
		if _, err := w.StartBox(&gomp4.BoxInfo{Type: typ}); err != nil {
			t.Fatalf("start box %s: %v", typ, err)
		}
	}
	end := func() {
		t.Helper()
		if _, err := w.EndBox(); err != nil {
			t.Fatalf("end box: %v", err)
		}
	}
	marshal := func(b gomp4.IImmutableBox) {
		t.Helper()
		if _, err := gomp4.Marshal(w, b, gomp4.Context{}); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	// flags24 把 24 位 box flags 转为 FullBox 需要的 3 字节大端表示。
	flags24 := func(v uint32) [3]byte {
		return [3]byte{byte(v >> 16), byte(v >> 8), byte(v)}
	}

	// ftyp
	start(gomp4.BoxTypeFtyp())
	marshal(&gomp4.Ftyp{
		MajorBrand: [4]byte{'i', 's', 'o', 'm'},
		CompatibleBrands: []gomp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	})
	end()

	// moov > mvhd + trak > (tkhd + mdia...) 最小化：只放 mvhd/tkhd/trex
	start(gomp4.BoxTypeMoov())
	start(gomp4.BoxTypeMvhd())
	marshal(&gomp4.Mvhd{Timescale: 90000, DurationV0: 0xFFFFFFFF})
	end()
	start(gomp4.BoxTypeTrak())
	start(gomp4.BoxTypeTkhd())
	marshal(&gomp4.Tkhd{TrackID: 1, DurationV0: 0xFFFFFFFF})
	end()
	start(gomp4.BoxTypeMdia())
	start(gomp4.BoxTypeMdhd())
	marshal(&gomp4.Mdhd{Timescale: 90000, DurationV0: 0xFFFFFFFF})
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
	marshal(&gomp4.Stsz{SampleSize: 0, SampleCount: 0})
	end()
	start(gomp4.BoxTypeStco())
	marshal(&gomp4.Stco{EntryCount: 0})
	end()
	end() // stbl
	end() // minf
	end() // mdia
	start(gomp4.BoxTypeTrex())
	marshal(&gomp4.Trex{
		TrackID:               1,
		DefaultSampleDuration: 3000,
		DefaultSampleFlags:    0,
	})
	end()
	end() // trak
	end() // moov

	for _, f := range frags {
		// moof > mfhd + traf > tfhd + tfdt + trun
		start(gomp4.BoxTypeMoof())
		start(gomp4.BoxTypeMfhd())
		marshal(&gomp4.Mfhd{SequenceNumber: uint32(f.baseDecodeTime)})
		end()
		start(gomp4.BoxTypeTraf())
		start(gomp4.BoxTypeTfhd())
		marshal(&gomp4.Tfhd{
			FullBox: gomp4.FullBox{Flags: flags24(gomp4.TfhdDefaultSampleFlagsPresent)},
			TrackID: 1,
			DefaultSampleFlags: 0,
		})
		end()
		start(gomp4.BoxTypeTfdt())
		marshal(&gomp4.Tfdt{
			FullBox:               gomp4.FullBox{Version: 1},
			BaseMediaDecodeTimeV1: f.baseDecodeTime,
		})
		end()
		start(gomp4.BoxTypeTrun())
		tr := &gomp4.Trun{
			FullBox:     gomp4.FullBox{Flags: flags24(0x000100 | 0x000400)}, // duration + sample_flags
			SampleCount: uint32(len(f.samples)),
			Entries:     make([]gomp4.TrunEntry, len(f.samples)),
		}
		for i, s := range f.samples {
			var flags uint32
			if !s.keyframe {
				flags = SampleFlagNonSync
			}
			tr.Entries[i] = gomp4.TrunEntry{
				SampleDuration: s.duration,
				SampleFlags:    flags,
			}
		}
		marshal(tr)
		end()
		end() // traf
		end() // moof

		// mdat：每样本 4 字节占位
		start(gomp4.BoxTypeMdat())
		payload := make([]byte, 4*len(f.samples))
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		end()
	}
	return mem.reader()
}

// memWriteSeeker 是支持真实 Seek 的内存 WriteSeeker：
// go-mp4 的 EndBox 需要回写 box 长度，因此 Seek 必须改变后续写入位置。
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
	var target int64
	switch whence {
	case 0:
		target = offset
	case 1:
		target = m.off + offset
	case 2:
		target = int64(len(m.data)) + offset
	}
	if target < 0 {
		return 0, bytes.ErrTooLarge
	}
	m.off = target
	return target, nil
}

func (m *memWriteSeeker) reader() *bytes.Reader { return bytes.NewReader(m.data) }

func TestInspectFragmentSummaries(t *testing.T) {
	frags := []fragSpec{
		{baseDecodeTime: 0, samples: []struct {
			duration uint32
			keyframe bool
		}{{3000, true}, {3000, false}}},
		{baseDecodeTime: 6000, samples: []struct {
			duration uint32
			keyframe bool
		}{{3000, false}, {3000, true}}},
	}
	r := buildFMP4(t, frags)
	in := &Inspector{Timescale: 90000}
	rep, err := in.Inspect(r)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(rep.Fragments) != 2 {
		t.Fatalf("fragments=%d", len(rep.Fragments))
	}
	if rep.Fragments[0].SampleCount != 2 || rep.Fragments[1].SampleCount != 2 {
		t.Fatalf("sample counts: %+v", rep.Fragments)
	}
	if rep.Fragments[0].BaseDecodeTime != 0 || rep.Fragments[1].BaseDecodeTime != 6000 {
		t.Fatalf("base decode times: %+v", rep.Fragments)
	}
	// 每片各一个关键帧。
	if rep.Fragments[0].KeyframeCount != 1 || rep.Fragments[1].KeyframeCount != 1 {
		t.Fatalf("keyframe counts: %+v", rep.Fragments)
	}
	if len(rep.KeyframeTimes) != 2 || rep.KeyframeTimes[1] != 6000 {
		t.Fatalf("keyframe times: %v", rep.KeyframeTimes)
	}
	// 连续时间戳：0 -> 6000（2*3000），无不连续。
	if len(rep.Discontinuities) != 0 {
		t.Fatalf("unexpected discontinuities: %+v", rep.Discontinuities)
	}
	// 关键帧间隔 6000 ticks。
	if rep.MaxKeyframeGap != 6000 {
		t.Fatalf("max keyframe gap=%d", rep.MaxKeyframeGap)
	}
}

func TestInspectDetectsDiscontinuity(t *testing.T) {
	frags := []fragSpec{
		{baseDecodeTime: 0, samples: []struct {
			duration uint32
			keyframe bool
		}{{3000, true}}},
		// 期望起点 3000，实际跳到 9000。
		{baseDecodeTime: 9000, samples: []struct {
			duration uint32
			keyframe bool
		}{{3000, true}}},
	}
	rep, err := (&Inspector{Timescale: 90000}).Inspect(buildFMP4(t, frags))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Discontinuities) != 1 {
		t.Fatalf("discontinuities=%v", rep.Discontinuities)
	}
	d := rep.Discontinuities[0]
	if d.Expected != 3000 || d.Actual != 9000 || d.NextIndex != 1 {
		t.Fatalf("discontinuity detail: %+v", d)
	}
}

func TestInspectKeyframeGapAcrossFragments(t *testing.T) {
	frags := []fragSpec{
		{baseDecodeTime: 0, samples: []struct {
			duration uint32
			keyframe bool
		}{{3000, true}}},
		{baseDecodeTime: 3000, samples: []struct {
			duration uint32
			keyframe bool
		}{{3000, false}}},
		{baseDecodeTime: 6000, samples: []struct {
			duration uint32
			keyframe bool
		}{{3000, false}}},
		{baseDecodeTime: 9000, samples: []struct {
			duration uint32
			keyframe bool
		}{{3000, true}}},
	}
	rep, err := (&Inspector{Timescale: 90000}).Inspect(buildFMP4(t, frags))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.KeyframeTimes) != 2 {
		t.Fatalf("keyframe times=%v", rep.KeyframeTimes)
	}
	if rep.MaxKeyframeGap != 9000 {
		t.Fatalf("gap=%d, want 9000 ticks (100ms)", rep.MaxKeyframeGap)
	}
}
