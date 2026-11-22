package parse

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"

	"fieldcut/internal/timeline"
)

func emsgBoxV1(tick uint64, dur uint32, id uint32, msg string) []byte {
	// go-mp4 v1.4.0 的 emsg v1 净荷由其字段钩子编排，直接用 Marshal 生成，
	// 避免手写字节顺序与库实现不一致。
	src := &mp4.Emsg{
		FullBox:          mp4.FullBox{Version: 1},
		SchemeIdUri:      "urn:test:cam",
		Value:            "v1",
		Timescale:        1000,
		PresentationTime: tick,
		EventDuration:    dur,
		Id:               id,
		MessageData:      []byte(msg),
	}
	var p bytes.Buffer
	if _, err := mp4.Marshal(&p, src, mp4.Context{}); err != nil {
		panic(err)
	}
	return box("emsg", p.Bytes())
}

func u64(v uint64) []byte {
	return []byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	}
}

func id3Box() []byte {
	// TIT2 帧，UTF-8 编码字节 0x03 + 文本。
	text := []byte{0x03, 'C', 'A', 'M', 'A'}
	frame := append([]byte("TIT2"), u32(uint32(len(text)))...)
	frame = append(frame, make([]byte, 2)...) // flags
	frame = append(frame, text...)
	header := append([]byte("ID3"), 0x04, 0x00, 0x00)
	header = append(header, syncsafeEncode(len(frame))...)
	return box("id3 ", append(header, frame...))
}

func syncsafeEncode(n int) []byte {
	return []byte{
		byte(n >> 21), byte(n >> 14), byte(n >> 7), byte(n),
	}
}

func TestParseEmsgAndID3(t *testing.T) {
	data := append(emsgBoxV1(2000, 500, 7, "cue-in"), id3Box()...)
	roots, rep := Walk(bytes.NewReader(data), DefaultLimits())
	if rep.HasFatal() {
		t.Fatal(rep.Errors)
	}
	evs := ParseEvents(bytes.NewReader(data), roots, nil, rep)
	var em, id3 *Event
	for i := range evs {
		switch evs[i].Kind {
		case EvEmsg:
			em = &evs[i]
		case EvID3:
			id3 = &evs[i]
		}
	}
	if em == nil || em.EventID != 7 || em.Text != "cue-in" {
		t.Fatalf("emsg parse wrong: %+v", em)
	}
	// timescale=1000，tick 2000 -> 2s；duration 500 -> 0.5s。
	if em.StartNs != 2_000_000_000 || em.EndNs != 2_500_000_000 {
		t.Fatalf("emsg timing wrong: %+v", em)
	}
	if id3 == nil || id3.Text != "TIT2=CAMA" {
		t.Fatalf("id3 parse wrong: %+v", id3)
	}
}

func TestEmsgCorruptIsRecoverable(t *testing.T) {
	bad := box("emsg", []byte{1, 0, 0}) // version 1 但净荷残缺
	roots, rep := Walk(bytes.NewReader(bad), DefaultLimits())
	evs := ParseEvents(bytes.NewReader(bad), roots, nil, rep)
	if len(evs) != 0 {
		t.Fatal("corrupt emsg must not emit event")
	}
	if rep.SkippedBoxes+countRecoverable(rep) == 0 {
		t.Fatal("corrupt emsg must be reported")
	}
}

func countRecoverable(rep *Report) int {
	n := 0
	for _, e := range rep.Errors {
		if e.Level == LevelRecoverable {
			n++
		}
	}
	return n
}

// 构造含 SEI NAL 的视频样本：AVCC 长度前缀 + NAL type 6。
func seiSample(text string) []byte {
	// SEI: ptype=5（单字节）, psize=16+len(text)，uuid(16)+text。
	uuid := make([]byte, 16)
	for i := range uuid {
		uuid[i] = byte(i + 1)
	}
	sei := []byte{5, byte(16 + len(text))}
	sei = append(sei, uuid...)
	sei = append(sei, []byte(text)...)
	nal := append([]byte{6}, sei...) // NAL header type 6
	return append(u32(uint32(len(nal))), nal...)
}

func TestScanSEI(t *testing.T) {
	sample := seiSample("marker-A")
	ti := videoTrackWithSamples(t, [][]byte{sample})
	rep := &Report{}
	evs := scanSEI(bytes.NewReader(sampleBytes(sample)), ti, rep)
	if len(evs) != 1 || evs[0].Kind != EvSEI || evs[0].Text != "marker-A" {
		t.Fatalf("sei wrong: %+v", evs)
	}
	if evs[0].TrackID != ti.TrackID || evs[0].RawTick != 0 {
		t.Fatal("sei must bind to source track and raw tick")
	}
}

// sampleBytes 把样本放到其声明偏移，便于 ReadAt/Seek 读取。
func sampleBytes(sample []byte) []byte {
	buf := make([]byte, 64+len(sample))
	copy(buf[64:], sample)
	return buf
}

func videoTrackWithSamples(t *testing.T, payloads [][]byte) *TrackInfo {
	clock, err := timeline.NewClock(11, timeline.KindVideo, 90000, 0)
	if err != nil {
		t.Fatal(err)
	}
	ti := &TrackInfo{TrackID: 11, Kind: "video", Codec: "avc1", Clock: clock}
	for i, p := range payloads {
		ti.Samples = append(ti.Samples, Sample{
			Index: uint32(i + 1), Offset: 64, Size: uint32(len(p)),
			TimeNs: int64(i) * 1_000_000_000, DurationNs: 1_000_000_000,
			RawTick: uint64(i) * 90000,
		})
	}
	return ti
}

func TestRTPIngestAndWrap(t *testing.T) {
	g := timeline.NewGraph()
	rt, err := NewRTPTrack(g, 21, timeline.KindVideo, 0xabc, 90000)
	if err != nil {
		t.Fatal(err)
	}
	dg1, err := MarshalPacket(0, 90000, 0xabc, true, 96, []byte{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	p1, err := rt.Ingest(dg1)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Ns != 1_000_000_000 || p1.RawTS != 90000 {
		t.Fatalf("rtp map wrong: %+v", p1)
	}
	// 制造 32 位回绕：时间戳从 0xffffffff 附近回到小值。
	dg2, _ := MarshalPacket(1, 0xffffffff, 0xabc, false, 96, []byte{3})
	dg3, _ := MarshalPacket(2, 99, 0xabc, true, 96, []byte{4})
	if _, err := rt.Ingest(dg2); err != nil {
		t.Fatal(err)
	}
	p3, err := rt.Ingest(dg3)
	if err != nil {
		t.Fatalf("wrap must be handled: %v", err)
	}
	if rt.Wraps() != 1 || p3.WrapCount != 1 {
		t.Fatalf("wrap count wrong: %d", rt.Wraps())
	}
	if p3.Ns <= p1.Ns {
		t.Fatal("timeline must stay monotonic across RTP wrap")
	}
}

func TestRTPLostSequence(t *testing.T) {
	g := timeline.NewGraph()
	rt, _ := NewRTPTrack(g, 22, timeline.KindAudio, 1, 48000)
	dg, _ := MarshalPacket(10, 0, 1, false, 97, nil)
	if _, err := rt.Ingest(dg); err != nil {
		t.Fatal(err)
	}
	dg2, _ := MarshalPacket(13, 4800, 1, false, 97, nil) // 缺 11,12
	if _, err := rt.Ingest(dg2); err != nil {
		t.Fatal(err)
	}
	if rt.Lost != 2 {
		t.Fatalf("lost count = %d want 2", rt.Lost)
	}
}

func TestRTPBadDatagram(t *testing.T) {
	g := timeline.NewGraph()
	rt, _ := NewRTPTrack(g, 23, timeline.KindAudio, 2, 48000)
	if _, err := rt.Ingest([]byte{0x00}); err == nil {
		t.Fatal("malformed RTP datagram must error")
	}
}
