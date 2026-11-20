package rtp

import (
	"bytes"
	"io"
	"testing"

	mp4 "github.com/abema/go-mp4"
	"github.com/pion/rtp"
)

// memWS 为内存 WriteSeeker，供 go-mp4 Writer 使用。
type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = int64(len(m.buf)) + off
	}
	return m.pos, nil
}

// buildMP4 构造最小 MP4：ftyp + moov(trak(mdia(mdhd, minf(stbl(stts,stsz,stco))))) + mdat。
func buildMP4(t *testing.T, timescale, sttsTotal, stszCount uint32, stszBytes []uint32, stco uint32, mdatSize int) []byte {
	t.Helper()
	m := &memWS{}
	w := mp4.NewWriter(m)
	ctx := mp4.Context{}
	put := func(boxType mp4.BoxType, box mp4.IImmutableBox) {
		t.Helper()
		if _, err := w.StartBox(&mp4.BoxInfo{Type: boxType}); err != nil {
			t.Fatal(err)
		}
		if _, err := mp4.Marshal(w, box, ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := w.EndBox(); err != nil {
			t.Fatal(err)
		}
	}
	put(mp4.BoxTypeFtyp(), &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}, MinorVersion: 1})
	containers := []mp4.BoxType{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()}
	for _, c := range containers {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: c}); err != nil {
			t.Fatal(err)
		}
	}
	put(mp4.BoxTypeMdhd(), &mp4.Mdhd{Timescale: timescale, DurationV0: 90000})
	put(mp4.BoxTypeStts(), &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: sttsTotal, SampleDelta: 1000}}})
	put(mp4.BoxTypeStsz(), &mp4.Stsz{SampleCount: stszCount, EntrySize: stszBytes})
	put(mp4.BoxTypeStco(), &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{stco}})
	for range containers {
		if _, err := w.EndBox(); err != nil {
			t.Fatal(err)
		}
	}
	put(mp4.BoxTypeMdat(), &mp4.Mdat{Data: make([]byte, mdatSize)})
	return m.buf
}

// goodMP4 两遍构造：先占位 stco=0，再回填真实 mdat 负载偏移。
func goodMP4(t *testing.T) []byte {
	sizes := []uint32{10, 10, 10}
	draft := buildMP4(t, 90000, 3, 3, sizes, 0, 30)
	idx := bytes.Index(draft, []byte("mdat"))
	if idx < 0 {
		t.Fatal("no mdat")
	}
	return buildMP4(t, 90000, 3, 3, sizes, uint32(idx+4), 30)
}

func TestReadMP4Good(t *testing.T) {
	info := ReadMP4(goodMP4(t))
	if info.Timescale != 90000 {
		t.Fatalf("timescale=%d", info.Timescale)
	}
	if info.SampleCount != 3 {
		t.Fatalf("samples=%d", info.SampleCount)
	}
	if len(info.Issues) != 0 {
		t.Fatalf("issues=%v", info.Issues)
	}
}

func TestReadMP4Corrupt(t *testing.T) {
	// stts 总数 3，stsz 计数 4 → count_mismatch；stco 越界 → sample_out_of_range
	data := buildMP4(t, 90000, 3, 4, []uint32{10, 10, 10, 10}, 1<<20, 30)
	info := ReadMP4(data)
	kinds := map[string]bool{}
	for _, i := range info.Issues {
		kinds[i.Kind] = true
	}
	if !kinds["count_mismatch"] || !kinds["sample_out_of_range"] {
		t.Fatalf("issues=%v", info.Issues)
	}
	if info.FirstErrorOffset < 0 {
		t.Fatal("no first error offset")
	}
	// 最早可定位位置必须落在 stts/stsz 盒偏移上
	sttsIdx := int64(bytes.Index(data, []byte("stts")) - 4)
	stszIdx := int64(bytes.Index(data, []byte("stsz")) - 4)
	want := sttsIdx
	if stszIdx < want {
		want = stszIdx
	}
	if info.FirstErrorOffset != want {
		t.Fatalf("first offset=%d want %d", info.FirstErrorOffset, want)
	}
}

func encodePackets(t *testing.T, seqs []uint16, tss []uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	for i := range seqs {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seqs[i], Timestamp: tss[i], SSRC: 42},
			Payload: []byte{0xAB, 0xCD},
		}
		if err := Encode(&buf, pkt); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestIngestClean(t *testing.T) {
	data := encodePackets(t, []uint16{100, 101, 102}, []uint32{1000, 2000, 3000})
	res := Ingest(data)
	if res.Packets != 3 || len(res.Issues) != 0 {
		t.Fatalf("packets=%d issues=%v", res.Packets, res.Issues)
	}
	if res.FirstErrorOffset != -1 {
		t.Fatalf("first offset=%d", res.FirstErrorOffset)
	}
}

func TestIngestCorrupt(t *testing.T) {
	seqs := []uint16{100, 101, 103, 103, 102, 104, 105, 106}
	tss := []uint32{1000, 2000, 3000, 3000, 2500, 0xFFFFFF00, 0x00000050, 100}
	res := Ingest(encodePackets(t, seqs, tss))
	// 缺包 102 一次；乱序的 102 归位后 102->104 再计一次前向跳号
	if res.SeqGaps != 2 || res.Duplicates != 1 || res.Reorders != 1 {
		t.Fatalf("gaps=%d dups=%d reorders=%d", res.SeqGaps, res.Duplicates, res.Reorders)
	}
	if res.TimestampWraps != 1 {
		t.Fatalf("wraps=%d", res.TimestampWraps)
	}
	var sawTSJump bool
	for _, i := range res.Issues {
		if i.Kind == "timestamp_jump" {
			sawTSJump = true
		}
	}
	if !sawTSJump {
		t.Fatalf("issues=%v", res.Issues)
	}
	// 首个错误为第三条记录（缺包），每条记录 2+12+2=16 字节
	if res.FirstErrorOffset != 32 {
		t.Fatalf("first offset=%d want 32", res.FirstErrorOffset)
	}
}

func TestIngestSeqWrap(t *testing.T) {
	res := Ingest(encodePackets(t, []uint16{65534, 65535, 0, 1}, []uint32{1, 2, 3, 4}))
	if len(res.Issues) != 0 {
		t.Fatalf("issues=%v", res.Issues)
	}
}
