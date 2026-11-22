package rtp

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	pionrtp "github.com/pion/rtp"

	"mediabridge/internal/fault"
	rtpdump "mediabridge/internal/fixture/rtpdump"
)

func writeDump(t *testing.T, payloads [][]byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "aux.rtpd")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := WriteDump(f, 0xDEADBEEF, payloads); err != nil {
		t.Fatal(err)
	}
	return path
}

func rtpPayload(t *testing.T, seq uint16, ts uint32, body []byte) []byte {
	t.Helper()
	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0xDEADBEEF,
		},
		Payload: body,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDemuxInOrder(t *testing.T) {
	var raw [][]byte
	for i := 0; i < 10; i++ {
		raw = append(raw, rtpPayload(t, uint16(i), uint32(i*3000), []byte{byte(i), 1, 2, 3}))
	}
	rd, err := Open(writeDump(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Demux(rd, Options{SampleRate: 90000, KeepPayload: true, TrackID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Duplicates != 0 || res.Stats.Lost != 0 {
		t.Fatalf("stats = %+v", res.Stats)
	}
	if len(res.Packets) != 10 {
		t.Fatalf("packets = %d", len(res.Packets))
	}
	if res.Packets[0].PTSNS != 0 {
		t.Fatalf("first pts = %d", res.Packets[0].PTSNS)
	}
	if res.Packets[1].PTSNS <= res.Packets[0].PTSNS {
		t.Fatal("timestamps not increasing")
	}
	if res.Track.ID != 7 || res.Track.Kind != "aux" {
		t.Fatalf("track = %+v", res.Track)
	}
}

func TestDemuxReordersAndDedupes(t *testing.T) {
	ordered := [][]byte{
		rtpPayload(t, 10, 0, []byte{10}),
		rtpPayload(t, 11, 3000, []byte{11}),
		rtpPayload(t, 12, 6000, []byte{12}),
		rtpPayload(t, 13, 9000, []byte{13}),
	}
	shuffled := [][]byte{ordered[0], ordered[2], ordered[1], ordered[1], ordered[3]}
	rd, err := Open(writeDump(t, shuffled))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Demux(rd, Options{SampleRate: 90000, ReorderWindow: 32})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 4 {
		t.Fatalf("packets = %d want 4", len(res.Packets))
	}
	for i, p := range res.Packets {
		if want := uint32(10 + i); p.Sequence != want {
			t.Fatalf("packet %d seq = %d want %d", i, p.Sequence, want)
		}
	}
	if res.Stats.Duplicates != 1 {
		t.Fatalf("duplicates = %d", res.Stats.Duplicates)
	}
	if res.Stats.Reordered == 0 {
		t.Fatal("expected reorder accounting")
	}
}

func TestDemuxAccountsLoss(t *testing.T) {
	var raw [][]byte
	// Sequence 20..29 with 23 and 24 missing.
	for i := uint16(20); i < 30; i++ {
		if i == 23 || i == 24 {
			continue
		}
		raw = append(raw, rtpPayload(t, i, uint32(i)*3000, []byte{byte(i)}))
	}
	rd, err := Open(writeDump(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Demux(rd, Options{SampleRate: 90000, ReorderWindow: 4})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Lost != 2 {
		t.Fatalf("lost = %d want 2", res.Stats.Lost)
	}
	if got := res.Stats.RecoveryRate(); got < 0.79 || got > 0.81 {
		t.Fatalf("recovery = %f want ~0.8", got)
	}
	if len(res.Packets) != 8 {
		t.Fatalf("delivered = %d", len(res.Packets))
	}
}

func TestDemuxHandlesSequenceWrap(t *testing.T) {
	var raw [][]byte
	for i := 0; i < 6; i++ {
		seq := uint16(65533 + i) // 65533,65534,65535,0,1,2
		raw = append(raw, rtpPayload(t, seq, uint32(i*3000), []byte{byte(i)}))
	}
	rd, err := Open(writeDump(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Demux(rd, Options{SampleRate: 90000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packets) != 6 {
		t.Fatalf("packets = %d", len(res.Packets))
	}
	for i := 1; i < len(res.Packets); i++ {
		if res.Packets[i].PTSNS <= res.Packets[i-1].PTSNS {
			t.Fatalf("non-monotonic at %d", i)
		}
	}
}

func TestDemuxEmptyDump(t *testing.T) {
	rd, err := Open(writeDump(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Demux(rd, Options{SampleRate: 90000})
	if fault.CodeOf(err) != fault.CodeNoPackets {
		t.Fatalf("code = %v", fault.CodeOf(err))
	}
}

func TestDemuxCorruptCRC(t *testing.T) {
	path := writeDump(t, [][]byte{rtpPayload(t, 1, 0, []byte{1, 2, 3})})
	// Flip a byte in the record body to break the CRC.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	rd, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Demux(rd, Options{SampleRate: 90000})
	if fault.CodeOf(err) != fault.CodeCorruptContainer {
		t.Fatalf("code = %v (%v)", fault.CodeOf(err), err)
	}
}

func TestReaderRejectsBadMagic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.rtpd")
	os.WriteFile(path, []byte("XXXX\x00\x01\x00\x00\x00\x00\x00\x00"), 0o644)
	if _, err := Open(path); err == nil {
		t.Fatal("bad magic should fail")
	}
}

func TestFixtureRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rep, err := rtpdump.Generate(dir, "aux.rtpd", rtpdump.Options{
		Seed: 41, Count: 500, Loss: 0.03, Reorder: 0.02, Duplicate: 0.01,
	})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := Open(rep.Path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Demux(rd, Options{SampleRate: 90000, KeepPayload: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Duplicates != int64(rep.Duplicated) {
		t.Fatalf("duplicates = %d want %d", res.Stats.Duplicates, rep.Duplicated)
	}
	if res.Stats.Lost != int64(rep.Dropped) {
		t.Fatalf("lost = %d want %d", res.Stats.Lost, rep.Dropped)
	}
	if res.Stats.Delivered != int64(rep.Delivered) {
		t.Fatalf("delivered = %d want %d", res.Stats.Delivered, rep.Delivered)
	}
}

// Ensure the dump format round-trips through the raw reader without the RTP
// decoder, guarding the framing independently of pion.
func TestDumpFraming(t *testing.T) {
	payloads := [][]byte{[]byte("hello"), []byte("world!")}
	path := writeDump(t, payloads)
	rd, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		rec, err := rd.Next()
		if err == io.EOF {
			if i != len(payloads) {
				t.Fatalf("read %d records", i)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(rec.Payload, payloads[i]) {
			t.Fatalf("record %d mismatch", i)
		}
	}
}
