package model

import (
	"errors"
	"strings"
	"testing"
)

func frame() Frame {
	return Frame{
		TrackID: "cam0", Seq: 5, RawTS: 12345, Width: 64, Height: 48,
		Payload: []byte{1, 2, 3, 4},
		Meta:    []Metadata{{Key: "label", Value: "a"}},
	}
}

func TestContentDigestIgnoresMetadataAndDimensions(t *testing.T) {
	fr := frame()
	base := fr.ContentDigest()
	fr.Meta = []Metadata{{Key: "label", Value: "b"}}
	fr.Width, fr.Height = 1920, 1080
	fr.Energy = []float64{0.9}
	fr.Keyframe = true
	if fr.ContentDigest() != base {
		t.Fatal("metadata or presentation change altered the content digest")
	}
	fr.Payload = []byte{1, 2, 3, 5}
	if fr.ContentDigest() == base {
		t.Fatal("payload change did not alter the content digest")
	}
}

func TestIntegrityDigestCoversIdentityAndPayload(t *testing.T) {
	fr := frame()
	base := fr.IntegrityChecksum()
	fr.Meta = []Metadata{{Key: "label", Value: "b"}}
	if fr.IntegrityChecksum() != base {
		t.Fatal("metadata change altered integrity digest")
	}
	for _, mutate := range []func(*Frame){
		func(f *Frame) { f.Seq++ },
		func(f *Frame) { f.RawTS++ },
		func(f *Frame) { f.Payload = append(f.Payload, 9) },
		func(f *Frame) { f.TrackID = "cam1" },
	} {
		cp := frame()
		mutate(&cp)
		if cp.IntegrityChecksum() == base {
			t.Fatalf("integrity digest ignored a change: %+v", cp)
		}
	}
}

func TestErrorExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{New(KindInput, "op", "bad"), 2},
		{New(KindCorrupt, "op", "bad"), 2},
		{New(KindAmbiguous, "op", "bad"), 2},
		{New(KindNotFound, "op", "bad"), 2},
		{New(KindInvariant, "op", "bad"), 1},
		{errors.New("plain"), 1},
		{Corruption("op", "cam", 7, 128, "checksum"), 2},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Errorf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestCorruptionErrorFormatting(t *testing.T) {
	err := Corruption("sequence.Parse", "cam0", 7, 128, "checksum mismatch")
	msg := err.Error()
	for _, want := range []string{"corrupt", "sequence.Parse", "track=cam0", "frame=7", "offset=128", "checksum mismatch"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	wrapped := Wrap(KindInput, "read", err)
	if !IsKind(wrapped, KindInput) {
		t.Fatal("wrapped error lost its kind")
	}
	if ExitCode(wrapped) != 2 {
		t.Fatalf("wrapped exit code = %d", ExitCode(wrapped))
	}
	if !errors.Is(errors.Unwrap(wrapped), err) {
		t.Fatal("unwrap chain broken")
	}
}

func TestValidatePayload(t *testing.T) {
	video := Frame{TrackID: "cam", Seq: 3, Width: 64, Height: 48,
		Payload: EncodeVideoPayload(64, 48, 3, []byte("frame"))}
	if err := ValidatePayload(KindVideo, &video); err != nil {
		t.Fatalf("valid video rejected: %v", err)
	}
	badTag := video
	badTag.Payload = []byte("nope-frame")
	if err := ValidatePayload(KindVideo, &badTag); !IsKind(err, KindCorrupt) {
		t.Fatalf("bad tag error = %v", err)
	}
	wrongDims := video
	wrongDims.Width = 128
	if err := ValidatePayload(KindVideo, &wrongDims); !IsKind(err, KindCorrupt) {
		t.Fatalf("dimension mismatch error = %v", err)
	}
	wrongSeq := video
	wrongSeq.Seq = 9
	if err := ValidatePayload(KindVideo, &wrongSeq); !IsKind(err, KindCorrupt) {
		t.Fatalf("frame index mismatch error = %v", err)
	}
	short := Frame{TrackID: "cam", Seq: 1, Payload: []byte{1}}
	if err := ValidatePayload(KindVideo, &short); !IsKind(err, KindCorrupt) {
		t.Fatalf("short payload error = %v", err)
	}
	audio := Frame{TrackID: "mic", Seq: 2, Payload: EncodeAudioPayload(2, 4, []byte("xx"))}
	if err := ValidatePayload(KindAudio, &audio); err != nil {
		t.Fatalf("valid audio rejected: %v", err)
	}
	if err := ValidatePayload(Kind(9), &audio); !IsKind(err, KindInput) {
		t.Fatalf("unknown kind error = %v", err)
	}
}
