package ingest

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"relay/internal/fixture"
	"relay/internal/media"
)

func TestParseHealthyFragment(t *testing.T) {
	raw := fixture.BuildFragment(fixture.FragmentSpec{
		TrackID: 101, Sequence: 6, Tfdt: 18000,
		Samples: []fixture.SampleSpec{
			{Duration: 3000, Payload: fixture.SamplePayload(101, 6, 1, 64), Sync: true},
			{Duration: 3000, Payload: fixture.SamplePayload(101, 6, 2, 48)},
		},
	})
	f, err := ParseFragment("mem", 0, raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if f.TrackID != 101 || f.Sequence != 6 || f.Tfdt != 18000 {
		t.Fatalf("header mismatch: %+v", f)
	}
	if len(f.Samples) != 2 || !f.Samples[0].Sync || f.Samples[1].Sync {
		t.Fatalf("sample flags wrong: %+v", f.Samples)
	}
	if int64(len(f.Mdat)) != 64+48 {
		t.Fatalf("mdat payload length wrong: %d", len(f.Mdat))
	}
}

func TestReaderTruncatedMdat(t *testing.T) {
	raw := fixture.BuildFragment(fixture.FragmentSpec{
		TrackID: 301, Sequence: 99, Tfdt: 12288,
		Samples: []fixture.SampleSpec{
			{Duration: 1024, Payload: fixture.SamplePayload(301, 99, 1, 64), Sync: true},
			{Duration: 1024, Payload: fixture.SamplePayload(301, 99, 2, 48)},
		},
	})
	cut := len(raw) - 40
	rd := NewReader(bytes.NewReader(raw[:cut]), "cut", int64(cut))
	ev, err := rd.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Fragment != nil || ev.Fault == nil {
		t.Fatalf("expected fault event, got %+v", ev)
	}
	if ev.Fault.Code != media.FaultTruncated || ev.Fault.Offset == 0 {
		t.Fatalf("expected truncated fault with offset, got %v", ev.Fault)
	}
	if !ev.Fault.Isolation() {
		t.Fatal("truncated fragment must be isolated")
	}
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after isolated fragment, got %v", err)
	}
}

func TestReaderSkipsUnknownBoxes(t *testing.T) {
	raw := fixture.BuildFragment(fixture.FragmentSpec{
		TrackID: 201, Sequence: 1, Tfdt: 0,
		Samples: []fixture.SampleSpec{
			{Duration: 1024, Payload: fixture.SamplePayload(201, 1, 1, 32), Sync: true},
			{Duration: 1024, Payload: fixture.SamplePayload(201, 1, 2, 32)},
		},
	})
	free := make([]byte, 8)
	binaryPut32(free[:4], 8)
	copy(free[4:], "free")
	stream := append(free, raw...)
	rd := NewReader(bytes.NewReader(stream), "mixed", int64(len(stream)))
	ev, err := rd.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Fragment == nil || ev.Fragment.TrackID != 201 {
		t.Fatalf("free box should be skipped, got %+v", ev)
	}
}

func TestSampleRangeBeyondMdat(t *testing.T) {
	// Corrupt the second sample size so trun claims bytes past mdat.
	raw := fixture.BuildFragment(fixture.FragmentSpec{
		TrackID: 101, Sequence: 2, Tfdt: 6000,
		Samples: []fixture.SampleSpec{
			{Duration: 3000, Payload: fixture.SamplePayload(101, 2, 1, 64), Sync: true},
			{Duration: 3000, Payload: fixture.SamplePayload(101, 2, 2, 48)},
		},
	})
	// traf prefix: moof(8)+traf(8)+mfhd(16)+tfhd(16)+tfdt(20)=68. Inside
	// trun: header(8)+fullbox(4)+count(4)+data_offset(4), then 16-byte
	// per-sample entries (dur,size,flags,cto); entry 2's size is +4 in.
	const trunStart = 8 + 8 + 16 + 16 + 20
	put := trunStart + 8 + 4 + 4 + 4 /*data offset*/ + 16 /*entry 1*/ + 4 /*entry2 size*/
	binaryPut32(raw[put:], uint32(len(raw)*4))
	_, err := ParseFragment("mem", 0, raw)
	var f *media.Fault
	if !errors.As(err, &f) || f.Code != media.FaultSampleRange {
		t.Fatalf("expected sample range fault, got %v", err)
	}
}

func binaryPut32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
