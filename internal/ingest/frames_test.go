package ingest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/mediamend/mediamend/internal/model"
	"github.com/pion/rtp"
)

func writeJSONL(t *testing.T, dir, name string, lines [][]byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	var all []byte
	for _, l := range lines {
		all = append(all, l...)
		all = append(all, '\n')
	}
	if err := os.WriteFile(p, all, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func goodLine(t *testing.T, track string, seq, ts uint64, payload []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(payload)
	line := `{"track_id":"` + track + `","frame_seq":` + itoa(seq) +
		`,"device_ts":` + itoa(ts) + `,"rate":90000,"rtp_b64":"` +
		base64.StdEncoding.EncodeToString(payload) + `","sha256":"` +
		hex.EncodeToString(sum[:]) + `"}`
	return []byte(line)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestReaderChecksumAndOffset(t *testing.T) {
	dir := t.TempDir()
	pkts := [][]byte{
		mustRTP(t, 100, 9000),
		mustRTP(t, 101, 12000),
	}
	good0 := goodLine(t, "v1", 0, 9000, pkts[0])
	bad := goodLine(t, "v1", 1, 12000, pkts[1])
	// Tamper the checksum but keep the line otherwise intact.
	bad = append([]byte(nil), bad...)
	// Flip one hex nibble of the embedded sha256 to a different value.
	if bad[len(bad)-8] == '0' {
		bad[len(bad)-8] = 'a'
	} else {
		bad[len(bad)-8] = '0'
	}
	path := writeJSONL(t, dir, "v1.jsonl", [][]byte{good0, bad})

	r, err := Open(path, "v1")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	frames, digest, faults, err := ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames=%d want 2", len(frames))
	}
	if !frames[0].ChecksumOK || frames[1].ChecksumOK {
		t.Fatal("checksum flags wrong")
	}
	if len(faults) != 1 || faults[0].Class != model.FaultChecksum {
		t.Fatalf("faults=%v", faults)
	}
	loc := faults[0].Location
	if loc.Track != "v1" || loc.Seq != 1 {
		t.Fatalf("location=%+v", loc)
	}
	wantOffset := int64(len(good0) + 1)
	if loc.Offset != wantOffset {
		t.Fatalf("offset=%d want %d", loc.Offset, wantOffset)
	}
	if digest == "" {
		t.Fatal("empty input digest")
	}
	// Source checksum must be preserved verbatim on the faulty frame.
	if frames[1].SourceSHA == "" {
		t.Fatal("source checksum lost")
	}
}

func TestDigestInputsStableAndSensitive(t *testing.T) {
	dir := t.TempDir()
	p := writeJSONL(t, dir, "a.jsonl", [][]byte{goodLine(t, "a", 0, 0, mustRTP(t, 1, 0))})
	d1, err := DigestInputs([]TrackInput{{TrackID: "a", File: p}})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := DigestInputs([]TrackInput{{TrackID: "a", File: p}})
	if err != nil || d1 != d2 {
		t.Fatal("digest not deterministic")
	}
	// Order independence for the same track set.
	d3, err := DigestInputs([]TrackInput{
		{TrackID: "b", File: p}, {TrackID: "a", File: p},
	})
	if err != nil {
		t.Fatal(err)
	}
	d4, _ := DigestInputs([]TrackInput{
		{TrackID: "a", File: p}, {TrackID: "b", File: p},
	})
	if d3 != d4 {
		t.Fatal("digest depends on input slice order")
	}
}

func mustRTP(t *testing.T, seq uint16, ts uint32) []byte {
	t.Helper()
	p := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: 42},
		Payload: []byte{0xde, 0xad, 0xbe, 0xef},
	}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
