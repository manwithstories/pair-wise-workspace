package adapter

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/abema/go-mp4"

	"avpipe/internal/media"
)

func mp4ReadBox(r io.ReadSeeker) (*mp4.BoxInfo, error) { return mp4.ReadBoxInfo(r) }

func TestMP4IndexRoundTrip(t *testing.T) {
	want := []IndexEntry{
		{Sequence: 0, PTS90k: 0, Offset: 28},
		{Sequence: 1, PTS90k: 3000, Offset: 140},
		{Sequence: 2, PTS90k: 6000, Offset: 252},
	}
	raw, err := WriteMP4Index(want)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	r := bytes.NewReader(raw)
	got, err := ReadMP4Index(r, int64(len(raw)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("rows=%d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMP4MissingBox(t *testing.T) {
	// A file containing only an ftyp box, no avpi box.
	var buf bytes.Buffer
	payload := append([]byte("isom"), make([]byte, 12)...)
	if err := writeRawBox(&buf, boxFTYP, payload); err != nil {
		t.Fatal(err)
	}
	_, err := ReadMP4Index(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err == nil || !strings.Contains(err.Error(), media.KindMissingIndex) {
		t.Fatalf("want missing_index, got %v", err)
	}
}

func TestMP4TruncatedPayload(t *testing.T) {
	raw, _ := WriteMP4Index([]IndexEntry{{Sequence: 1}})
	// Cut the avpi payload mid-way.
	cut := raw[:len(raw)-10]
	_, err := ReadMP4Index(bytes.NewReader(cut), int64(len(cut)))
	if err == nil {
		t.Fatal("expected error on truncated mp4")
	}
}

func TestMP4UnknownVersion(t *testing.T) {
	raw, _ := WriteMP4Index([]IndexEntry{{Sequence: 1}})
	// Locate the avpi payload version byte (first byte after the 8-byte box
	// header, following the ftyp box).
	r := bytes.NewReader(raw)
	var ftypOff int64
	for {
		bi, err := mp4ReadBox(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if bi.Type == boxAVPI {
			raw[int64(bi.Offset)+int64(bi.HeaderSize)] = 77
			break
		}
		ftypOff = int64(bi.Offset + bi.Size)
		_, _ = r.Seek(ftypOff, io.SeekStart)
	}
	_, err := ReadMP4Index(bytes.NewReader(raw), int64(len(raw)))
	if err == nil || !strings.Contains(err.Error(), media.KindUnknownVersion) {
		t.Fatalf("want unknown_version, got %v", err)
	}
}
