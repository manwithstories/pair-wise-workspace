package ingest

import (
	"errors"
	"testing"

	"github.com/clearliu/fieldcut/internal/clock"
)

// fullBox writes an ISOBMFF full box: 8-byte box header followed by the
// 4-byte version/flags field and the payload.
func fullBox(b *BoxBuilder, typ string, version byte, flags uint32, payload []byte) {
	vf := []byte{
		version,
		byte(flags >> 16),
		byte(flags >> 8),
		byte(flags),
	}
	b.Box(typ, append(vf, payload...))
}

// buildInitSegment builds a structurally valid init segment shell:
// ftyp + moov(mvhd). The timeline/index layers only need the box tree.
func buildInitSegment() *BoxBuilder {
	b := &BoxBuilder{}
	b.Box("ftyp", []byte("iso5dash"))
	moov := &BoxBuilder{}
	fullBox(moov, "mvhd", 0, 0, make([]byte, 96))
	b.Container("moov", moov)
	return b
}

// buildMediaSegment builds styp + moof(tfhd,trun) + mdat. The trun uses a
// real full-box layout so go-mp4 can unmarshal it.
func buildMediaSegment() *BoxBuilder {
	b := &BoxBuilder{}
	b.Box("styp", []byte("msdhmsdh"))
	moof := &BoxBuilder{}
	fullBox(moof, "tfhd", 0, 0x020000, []byte{0, 0, 0, 1}) // track id 1
	trunPayload := []byte{
		0, 0, 0, 2, // sample_count = 2
		0, 0, 10, 0, // sample 0 duration
		0, 0, 0, 10, // sample 0 size
		0, 0, 10, 0, // sample 1 duration
		0, 0, 0, 10, // sample 1 size
	}
	fullBox(moof, "trun", 0, 0x000300, trunPayload)
	b.Container("moof", moof)
	b.Box("mdat", make([]byte, 20))
	return b
}

func TestValidateInitSegment(t *testing.T) {
	rep, err := ValidateSegment(buildInitSegment().Bytes())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.Kind != KindInitSegment || !rep.HasMOOV || rep.Fragments != 0 {
		t.Fatalf("report wrong: %+v", rep)
	}
	if rep.BoxCount != 3 { // ftyp, moov, mvhd
		t.Fatalf("box count = %d, want 3: %v", rep.BoxCount, rep.Boxes)
	}
}

func TestValidateMediaSegment(t *testing.T) {
	rep, err := ValidateSegment(buildMediaSegment().Bytes())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.Kind != KindMediaSegment || rep.Fragments != 1 || !rep.HasSTYP {
		t.Fatalf("report wrong: %+v", rep)
	}
	if rep.PayloadBytes != 20 {
		t.Fatalf("payload bytes = %d, want 20", rep.PayloadBytes)
	}
}

func TestValidateTruncatedBox(t *testing.T) {
	full := buildMediaSegment().Bytes()
	cases := map[string][]byte{
		"cut mid box":      full[:len(full)-8],
		"header only":      full[:8],
		"empty":            {},
		"declared too big": full[:4], // size field with no box type/payload
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateSegment(data); !errors.Is(err, clock.ErrTruncatedBox) {
				t.Fatalf("%s: want ErrTruncatedBox, got %v", name, err)
			}
		})
	}
}

func TestValidateMissingMandatoryBoxes(t *testing.T) {
	// moof without trun must be rejected.
	b := &BoxBuilder{}
	b.Box("styp", []byte("msdhmsdh"))
	moof := &BoxBuilder{}
	fullBox(moof, "tfhd", 0, 0x020000, []byte{0, 0, 0, 1})
	b.Container("moof", moof)
	b.Box("mdat", make([]byte, 4))
	if _, err := ValidateSegment(b.Bytes()); !errors.Is(err, clock.ErrTruncatedBox) {
		t.Fatalf("missing trun: want ErrTruncatedBox, got %v", err)
	}

	// media segment without styp.
	noStyp := &BoxBuilder{}
	m2 := &BoxBuilder{}
	fullBox(m2, "tfhd", 0, 0x020000, []byte{0, 0, 0, 1})
	fullBox(m2, "trun", 0, 0x000300, []byte{0, 0, 0, 1, 0, 0, 10, 0, 0, 0, 0, 10})
	noStyp.Container("moof", m2)
	noStyp.Box("mdat", make([]byte, 10))
	if _, err := ValidateSegment(noStyp.Bytes()); !errors.Is(err, clock.ErrTruncatedBox) {
		t.Fatalf("missing styp: want ErrTruncatedBox, got %v", err)
	}
}

func TestValidateOversizeRejected(t *testing.T) {
	// A box header claiming more bytes than the segment provides.
	b := &BoxBuilder{}
	b.Box("ftyp", []byte("iso5dash"))
	raw := b.Bytes()
	// Patch ftyp size to claim 1 GiB.
	raw[0], raw[1], raw[2], raw[3] = 0x40, 0, 0, 0
	if _, err := ValidateSegment(raw); !errors.Is(err, clock.ErrTruncatedBox) {
		t.Fatalf("oversize declaration: want ErrTruncatedBox, got %v", err)
	}
}

func TestBoxBuilderDeterministic(t *testing.T) {
	a := buildMediaSegment().Bytes()
	c := buildMediaSegment().Bytes()
	if len(a) != len(c) {
		t.Fatal("builder length differs across runs")
	}
	for i := range a {
		if a[i] != c[i] {
			t.Fatalf("builder non-deterministic at byte %d", i)
		}
	}
	if got := buildMediaSegment().Truncate(12); len(got) != 12 {
		t.Fatalf("truncate len = %d", len(got))
	}
}
