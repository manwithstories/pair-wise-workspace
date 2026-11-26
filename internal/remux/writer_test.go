package remux

import (
	"bytes"
	"io"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

func testFrags() []OutFrag {
	return []OutFrag{
		{TrackID: 1, TFDT: 90000, Samples: []OutSample{
			{Duration: 3000, Size: 3, Flags: 0x2000000, CTO: 6000, Data: []byte("aaa")},
			{Duration: 3000, Size: 4, Flags: 0x1010000, CTO: 0, Data: []byte("bbbb")},
		}},
		{TrackID: 2, TFDT: 48000, Samples: []OutSample{
			{Duration: 960, Size: 2, Data: []byte("cc")},
		}},
	}
}

func TestMarshalSegmentRoundTrip(t *testing.T) {
	m := &memWS{}
	hashes, err := MarshalSegment(m, 7, testFrags())
	if err != nil {
		t.Fatal(err)
	}
	if got := hashes[1]; got != HashPayload([][]byte{[]byte("aaa"), []byte("bbbb")}) {
		t.Fatalf("track 1 payload hash mismatch")
	}
	var (
		seq      uint32
		tfdts    []uint64
		offsets  []int64
		entries  int
		moofOff  int64
		mdatData []byte
		mdatOff  int64
	)
	r := bytes.NewReader(m.buf)
	_, err = mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoof():
			moofOff = int64(h.BoxInfo.Offset)
			return h.Expand()
		case mp4.BoxTypeTraf():
			return h.Expand()
		case mp4.BoxTypeMfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			seq = box.(*mp4.Mfhd).SequenceNumber
		case mp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfdts = append(tfdts, box.(*mp4.Tfdt).GetBaseMediaDecodeTime())
		case mp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*mp4.Trun)
			offsets = append(offsets, moofOff+int64(trun.DataOffset))
			entries += len(trun.Entries)
		case mp4.BoxTypeMdat():
			mdatOff = int64(h.BoxInfo.Offset + h.BoxInfo.HeaderSize)
			buf := new(bytes.Buffer)
			if _, err := h.ReadData(buf); err != nil {
				return nil, err
			}
			mdatData = buf.Bytes()
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 7 {
		t.Fatalf("mfhd seq=%d", seq)
	}
	if len(tfdts) != 2 || tfdts[0] != 90000 || tfdts[1] != 48000 {
		t.Fatalf("tfdts=%v", tfdts)
	}
	if entries != 3 {
		t.Fatalf("entries=%d", entries)
	}
	// data offsets must point at each track's payload inside mdat
	if len(offsets) != 2 || offsets[0] != mdatOff || offsets[1] != mdatOff+7 {
		t.Fatalf("offsets=%v mdatOff=%d", offsets, mdatOff)
	}
	if string(mdatData) != "aaabbbbcc" {
		t.Fatalf("mdat=%q", mdatData)
	}
}

func TestMarshalSegmentRejectsBadSize(t *testing.T) {
	m := &memWS{}
	frags := []OutFrag{{TrackID: 1, Samples: []OutSample{{Size: 5, Data: []byte("x")}}}}
	if _, err := MarshalSegment(m, 1, frags); err == nil {
		t.Fatal("expected size mismatch error")
	}
}

func TestMemWSSeek(t *testing.T) {
	m := &memWS{}
	m.Write([]byte("hello"))
	if _, err := m.Seek(1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	m.Write([]byte("A"))
	if string(m.buf) != "hAllo" {
		t.Fatalf("buf=%q", m.buf)
	}
}
