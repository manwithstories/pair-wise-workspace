package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"mediacheck/internal/adapter"
	"mediacheck/internal/model"
)

// ---- 最小 fMP4 字节级写入夹具（仅测试使用） ----
// 约定：fullBox 的 payload 参数只放 verflags 之后的字段，verflags 由 fullBox 统一写。

func box(typ string, payload []byte) []byte {
	var b bytes.Buffer
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(8+len(payload)))
	copy(hdr[4:], typ)
	b.Write(hdr[:])
	b.Write(payload)
	return b.Bytes()
}

func fullBox(typ string, flags uint32, fields []byte) []byte {
	body := make([]byte, 4+len(fields))
	binary.BigEndian.PutUint32(body[:4], flags)
	copy(body[4:], fields)
	return box(typ, body)
}

func u32b(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// audioInit 构造单 ipcm 音频轨初始化段。
func audioInit(trackID uint32, timescale, rate uint32, channels uint16, colr *model.ColorMeta) []byte {
	ftyp := box("ftyp", append(append([]byte("iso5"), make([]byte, 4)...), []byte("iso5dash")...))
	// tkhd v0 字段（verflags 之后）：creation(4)+modification(4)+track_ID(4)
	tkhdFields := make([]byte, 12)
	binary.BigEndian.PutUint32(tkhdFields[8:12], trackID)
	tkhd := fullBox("tkhd", 0x000007, tkhdFields)
	// mdhd v0 字段：creation(4)+modification(4)+timescale(4)+duration(4)
	mdhdFields := make([]byte, 16)
	binary.BigEndian.PutUint32(mdhdFields[8:12], timescale)
	mdhd := fullBox("mdhd", 0, mdhdFields)
	// hdlr 字段：pre_defined(4)+handler_type(4)
	hdlr := fullBox("hdlr", 0, append(make([]byte, 4), []byte("soun")...))
	// ipcm sample entry 体：reserved(6)+dqi(2)+channels(2)+samplesize(2)+pre(2)+reserved(4)+samplerate(4)
	entryBody := make([]byte, 20)
	binary.BigEndian.PutUint16(entryBody[8:10], channels)
	binary.BigEndian.PutUint16(entryBody[10:12], 16)
	binary.BigEndian.PutUint32(entryBody[16:20], rate<<16)
	if colr != nil {
		cp := make([]byte, 11)
		copy(cp[0:4], "nclx")
		binary.BigEndian.PutUint16(cp[4:6], colr.ColorPrimaries)
		binary.BigEndian.PutUint16(cp[6:8], colr.Transfer)
		binary.BigEndian.PutUint16(cp[8:10], colr.Matrix)
		if colr.FullRange {
			cp[10] = 1
		}
		entryBody = append(entryBody, box("colr", cp)...)
	}
	// stsd 字段（verflags 之后）：entry_count(4)+sample entry box...
	stsdFields := append(u32b(1), box("ipcm", entryBody)...)
	stsd := fullBox("stsd", 0, stsdFields)
	minf := box("minf", stsd)
	mdia := box("mdia", append(append(mdhd, hdlr...), minf...))
	trak := box("trak", append(tkhd, mdia...))
	moov := box("moov", trak)
	return append(ftyp, moov...)
}

// audioFragment 构造 moof(mfhd,traf(tfhd,tfdt,trun))+mdat。
func audioFragment(seq uint32, trackID, timescale, baseTime uint32, payload []byte, dur uint32) []byte {
	mfhd := fullBox("mfhd", 0, u32b(seq))         // 字段：sequence_number
	tfhd := fullBox("tfhd", 0, u32b(trackID))     // 字段：track_ID
	tfdtBox := fullBox("tfdt", 0, u32b(baseTime)) // 字段：baseMediaDecodeTime(v0)
	trunFlags := uint32(trunDataOffset | trunSampleDuration | trunSampleSize)
	trunFields := append(u32b(1), u32b(0)...) // sample_count + data_offset 占位
	trunFields = append(trunFields, u32b(dur)...)
	trunFields = append(trunFields, u32b(uint32(len(payload)))...)
	trun := fullBox("trun", trunFlags, trunFields)
	traf := box("traf", append(append(tfhd, tfdtBox...), trun...))
	moof := box("moof", append(mfhd, traf...))
	// data_offset 从 moof 起点指向 mdat payload（moof 长度 + mdat 8 字节头）。
	// trun 字段内 data_offset 位置：trun box 头(8)+verflags(4)+count(4) → moof 内偏移再加 4。
	trunPos := len(moof) - len(trun)
	dataOffPos := trunPos + 8 + 4 + 4
	binary.BigEndian.PutUint32(moof[dataOffPos:dataOffPos+4], uint32(len(moof)+8))
	mdat := box("mdat", payload)
	return append(moof, mdat...)
}

func TestParseInitAndFragment(t *testing.T) {
	init := audioInit(1, 48000, 48000, 2, &model.ColorMeta{ColorPrimaries: 1, Transfer: 1, Matrix: 1})
	f1 := audioFragment(1, 1, 48000, 0, make([]byte, 960*2*2), 960)
	f2 := audioFragment(2, 1, 48000, 960, make([]byte, 960*2*2), 960)
	p := New()
	res, err := p.Parse(context.Background(), adapter.ParseInput{InitSegment: init, Fragments: [][]byte{f1, f2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tracks) != 1 {
		t.Fatalf("tracks = %d", len(res.Tracks))
	}
	tm := res.Tracks[0]
	if tm.SampleRate != 48000 || tm.Layout != model.LayoutStereo || tm.Codec != "ipcm" {
		t.Fatalf("track meta = %+v", tm)
	}
	if tm.Color == nil || tm.Color.ColorPrimaries != 1 {
		t.Fatalf("colr not parsed: %+v", tm.Color)
	}
	if len(res.Fragments) != 2 || res.Fragments[0].Sequence != 1 {
		t.Fatalf("fragments = %+v", res.Fragments)
	}
	if res.Fragments[1].BaseMediaDecodeTime != 960 {
		t.Fatalf("tfdt = %d", res.Fragments[1].BaseMediaDecodeTime)
	}
	if len(res.Fragments[0].Samples) != 1 || len(res.Fragments[0].Samples[0].Data) != 960*2*2 {
		t.Fatal("sample extraction failed")
	}
	if res.Fragments[0].Intervals[0].Duration().Ticks(48000) != 960 {
		t.Fatal("interval duration mismatch")
	}
	var unknown int
	for _, bs := range res.Boxes {
		if bs.Unknown {
			unknown++
		}
	}
	if unknown != 0 {
		t.Fatalf("fixture should contain only known boxes, got %d", unknown)
	}
}

func TestTruncatedBoxRejectedWithOffset(t *testing.T) {
	good := audioFragment(1, 1, 48000, 0, []byte{1, 2, 3, 4}, 4)
	bad := good[:len(good)-3]
	p := New()
	_, err := p.Parse(context.Background(), adapter.ParseInput{Fragments: [][]byte{bad}})
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrTruncatedBox {
		t.Fatalf("want truncated_box, got %v", err)
	}
	if me.Offset < 0 {
		t.Fatal("truncation error must carry byte offset")
	}
}

func TestMissingMfhdRejected(t *testing.T) {
	f := audioFragment(3, 1, 48000, 0, []byte{1}, 1)
	idx := bytes.Index(f, []byte("mfhd"))
	if idx < 0 {
		t.Fatal("mfhd marker missing")
	}
	copy(f[idx:idx+4], "xyz0")
	p := New()
	_, err := p.Parse(context.Background(), adapter.ParseInput{Fragments: [][]byte{f}})
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrMissingMFHD {
		t.Fatalf("want missing_mfhd, got %v", err)
	}
}

func TestUnknownLayoutReportedByTrack(t *testing.T) {
	init := audioInit(7, 48000, 48000, 3, nil) // 3 声道不在白名单
	p := New()
	res, err := p.Parse(context.Background(), adapter.ParseInput{InitSegment: init})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tracks[0].Layout != "" {
		t.Fatalf("unknown layout must map to empty, got %q", res.Tracks[0].Layout)
	}
}

func TestUnknownBoxRetainedInSummary(t *testing.T) {
	good := audioFragment(1, 1, 48000, 0, []byte{1, 2, 3, 4}, 4)
	// 在分片尾部追加一个 go-mp4 不识别的自定义 box（xyz0），不破坏 moof/mdat 边界。
	custom := box("xyz0", []byte{0xde, 0xad})
	withUnknown := append(append([]byte{}, good...), custom...)
	init := audioInit(1, 48000, 48000, 2, nil)
	p := New()
	res, err := p.Parse(context.Background(), adapter.ParseInput{InitSegment: init, Fragments: [][]byte{withUnknown}})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, bs := range res.Fragments[0].Boxes {
		if bs.Type == "xyz0" && bs.Unknown {
			found = true
		}
	}
	if !found {
		t.Fatal("unknown box must be retained and flagged in summary")
	}
}
