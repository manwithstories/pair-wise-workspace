package adapter

import (
	"bytes"
	"io"
	"testing"

	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

type stub struct {
	format  string
	version int
	magic   string
}

func (s stub) Format() string { return s.format }
func (s stub) Version() int   { return s.version }
func (s stub) Name() string   { return s.format }
func (s stub) Sniff(r io.ReadSeeker) bool {
	pos, _ := r.Seek(0, io.SeekCurrent)
	defer r.Seek(pos, io.SeekStart)
	var b [4]byte
	n, _ := io.ReadFull(r, b[:])
	return n == 4 && string(b[:]) == s.magic
}
func (s stub) Parse(src Source, opts Options) (*media.Asset, error) {
	return &media.Asset{ID: src.AssetID, Adapter: s.format, Version: s.version}, nil
}

func TestRegistryResolveVersions(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stub{"mp4", 1, "ftyp"})
	r.MustRegister(stub{"mp4", 2, "ftyp"})
	r.MustRegister(stub{"rtp", 1, "RTP1"})

	if got := r.Versions("mp4"); len(got) != 2 || got[1] != 2 {
		t.Fatalf("versions = %v", got)
	}
	a, err := r.Resolve("mp4", 1, Source{Path: "a.mp4"})
	if err != nil || a.Version() != 1 {
		t.Fatalf("pinned resolve: %v", err)
	}
	a, err = r.Resolve("mp4", 0, Source{Path: "a.mp4"})
	if err != nil || a.Version() != 2 {
		t.Fatalf("latest resolve: %v (v=%d)", err, a.Version())
	}
}

func TestRegistryUnknownVersionIsLocated(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stub{"mp4", 1, "ftyp"})
	_, err := r.Resolve("mp4", 9, Source{Path: "/archive/clip.mov", AssetID: "A7"})
	if err == nil {
		t.Fatal("want error")
	}
	e, ok := fault.As(err)
	if !ok || e.Code != fault.CodeUnknownVersion {
		t.Fatalf("code = %v", err)
	}
	if e.Loc.SourcePath != "/archive/clip.mov" || e.Loc.AssetID != "A7" {
		t.Fatalf("locator = %v", e.Loc)
	}
	if _, err := r.Resolve("webm", 1, Source{Path: "x"}); fault.CodeOf(err) != fault.CodeUnknownVersion {
		t.Fatalf("unknown format code = %v", err)
	}
}

func TestRegistryDuplicateRejected(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stub{"mp4", 1, "ftyp"})
	if err := r.Register(stub{"mp4", 1, "ftyp"}); err == nil {
		t.Fatal("want duplicate rejection")
	}
	if err := r.Register(stub{"", 1, ""}); err == nil {
		t.Fatal("want invalid format rejection")
	}
}

func TestRegistrySniff(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stub{"mp4", 1, "ftyp"})
	r.MustRegister(stub{"rtp", 1, "RTP1"})
	a, ok := r.Sniff(bytes.NewReader([]byte("RTP1payload")))
	if !ok || a.Format() != "rtp" {
		t.Fatalf("sniff rtp = %v %v", a, ok)
	}
	a, ok = r.Sniff(bytes.NewReader([]byte("ftypxxxxx")))
	if !ok || a.Format() != "mp4" {
		t.Fatalf("sniff mp4 = %v %v", a, ok)
	}
	if _, ok := r.Sniff(bytes.NewReader([]byte("junk"))); ok {
		t.Fatal("should not sniff junk")
	}
}

func TestFormatsSorted(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stub{"rtp", 1, "RTP1"})
	r.MustRegister(stub{"mp4", 1, "ftyp"})
	got := r.Formats()
	if len(got) != 2 || got[0] != "mp4" || got[1] != "rtp" {
		t.Fatalf("formats = %v", got)
	}
}
