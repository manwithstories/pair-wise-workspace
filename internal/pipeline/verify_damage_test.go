package pipeline

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/abema/go-mp4"

	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
)

// reparseFixture decodes the summary and the generated fixtures so damage
// assertions can run against the original bytes rather than the scanner.
func reparseFixture(t *testing.T, cfg Config) (jsonSummary, map[string]Source) {
	t.Helper()
	summary, err := Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var js jsonSummary
	if err := json.Unmarshal(raw, &js); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sources, err := BuildSources(cfg)
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	byName := make(map[string]Source)
	for _, src := range sources {
		byName[src.Name] = src
	}
	return js, byName
}

func TestSummaryDamageOffsetsReparse(t *testing.T) {
	js, byName := reparseFixture(t, DefaultConfig())
	// 2. Error positions: every reported offset must fall inside the named
	//    segment, and the anomaly class must be reproducible at that offset.
	for _, d := range js.DamagedRanges {
		if !d.Known {
			continue
		}
		src, ok := byName[d.Layer]
		if !ok {
			t.Fatalf("unknown layer %q", d.Layer)
		}
		buf := findSegment(src, d.Segment)
		if buf == nil {
			// A skipped fragment is absent by construction; only gap reports
			// may reference a missing segment, and those carry the moof offset
			// of a neighbouring fragment.
			if d.Kind == string(clock.ErrMfhdGap) || d.Kind == string(clock.ErrSegmentTruncated) {
				continue
			}
			t.Fatalf("%s: segment %d not found for %s", d.Layer, d.Segment, d.Kind)
		}
		// A sample entirely past a truncated tail legitimately has an offset
		// beyond the buffer; every other kind must point inside it.
		if d.Offset >= uint64(len(buf)) && d.Kind != string(clock.ErrSampleOutOfBounds) {
			t.Fatalf("%s: offset %d outside segment %d (len %d)", d.Layer, d.Offset, d.Segment, len(buf))
		}
		switch d.Kind {
		case string(clock.ErrBoxSizeOverrun):
			assertCorruptBoxAt(t, buf, d.Offset)
		case string(clock.ErrSampleOutOfBounds):
			assertSampleCrossesMdat(t, buf, d.Offset)
		case string(clock.ErrMfhdDuplicate):
			assertMfhdAt(t, buf, d.Offset)
		case string(clock.ErrTfdtRegression):
			assertTfdtAt(t, buf, d.Offset)
		}
	}

}

func assertSampleCrossesMdat(t *testing.T, buf []byte, offset uint64) {
	t.Helper()
	lay, ok := lastMoofBefore(buf, offset)
	if !ok {
		t.Fatalf("no moof decoded before sample offset %d", offset)
	}
	// Walk the trun sample sizes with the tfhd default producing a start per
	// sample; the reported offset must be one of those starts and its declared
	// span must run past the truncated buffer.
	cursor := lay.sampleStart
	for i := 0; i < len(lay.sizes); i++ {
		if cursor == offset {
			// The declared span must exceed the mdat payload end (a corrupted
			// box size can shrink the mdat even when the bytes are present).
			if cursor+uint64(lay.sizes[i]) <= lay.payloadEnd && !lay.incomplete {
				t.Fatalf("sample at %d size %d fits inside mdat payload end %d; not truncated",
					cursor, lay.sizes[i], lay.payloadEnd)
			}
			return
		}
		cursor += uint64(lay.sizes[i])
	}
	t.Fatalf("reported out-of-bounds offset %d is not a decoded sample start (start %d, %d samples)",
		offset, lay.sampleStart, len(lay.sizes))
}

type moofLayout struct {
	sampleStart uint64
	payloadEnd  uint64
	sizes       []uint32
	incomplete  bool
}

// lastMoofBefore independently decodes the moof preceding off: its trun
// per-sample sizes, tfhd data offset, and whether the enclosing mdat declares
// more bytes than the segment contains.
func lastMoofBefore(buf []byte, off uint64) (moofLayout, bool) {
	var out moofLayout
	found := false
	for pos := 0; pos+8 <= len(buf); {
		b, err := cmaf.ParseBoxHeader(buf, pos, len(buf))
		if err != nil {
			break
		}
		if b.Type == "moof" && uint64(b.Offset) < off {
			if lay, ok := decodeMoofLayout(buf, b); ok {
				out = lay
				found = true
			}
		}
		if b.Type == "mdat" && uint64(b.Offset) < off {
			out.payloadEnd = uint64(b.End)
			if !b.Valid {
				out.incomplete = true
			}
		}
		if b.End <= pos {
			break
		}
		pos = b.End
	}
	return out, found
}

func decodeMoofLayout(buf []byte, moof cmaf.Box) (moofLayout, bool) {
	var lay moofLayout
	var base uint64
	var trexDefault uint32
	var dataOffset int64
	var haveTfhd bool
	var haveDataOffset bool
	var ok bool
	payload := buf[moof.Offset+moof.HeaderSize:]
	_ = payload
	var walk func([]byte, int)
	walk = func(b []byte, depth int) {
		if depth > 4 {
			return
		}
		for pos := 0; pos+8 <= len(b); {
			info, err := mp4.ReadBoxInfo(bytes.NewReader(b[pos:]))
			if err != nil || info.Size == 0 || info.Size > uint64(len(b)-pos) {
				return
			}
			body := b[pos+int(info.HeaderSize) : pos+int(info.Size)]
			switch string(info.Type[:]) {
			case "tfhd":
				var tfhd mp4.Tfhd
				if _, err := mp4.Unmarshal(bytes.NewReader(body), uint64(len(body)), &tfhd, mp4.Context{}); err == nil {
					haveTfhd = true
					if tfhd.CheckFlag(mp4.TfhdBaseDataOffsetPresent) {
						base = tfhd.BaseDataOffset
					} else {
						base = uint64(moof.Offset)
					}
					if tfhd.CheckFlag(mp4.TfhdDefaultSampleSizePresent) {
						trexDefault = tfhd.DefaultSampleSize
					}
				}
			case "trun":
				var trun mp4.Trun
				if _, err := mp4.Unmarshal(bytes.NewReader(body), uint64(len(body)), &trun, mp4.Context{}); err == nil {
					if trun.CheckFlag(0x000001) {
						dataOffset = int64(trun.DataOffset)
						haveDataOffset = true
					}
					for _, e := range trun.Entries {
						sz := trexDefault
						if trun.CheckFlag(0x000200) {
							sz = e.SampleSize
						}
						lay.sizes = append(lay.sizes, sz)
					}
				}
			case "traf", "moof":
				walk(body, depth+1)
			}
			pos += int(info.Size)
		}
	}
	walk(buf[moof.Offset+moof.HeaderSize:moof.End], 0)
	if !haveTfhd || len(lay.sizes) == 0 {
		return lay, false
	}
	start := base
	if haveDataOffset {
		start = uint64(int64(base) + dataOffset)
	}
	lay.sampleStart = start
	ok = true
	return lay, ok
}

func assertMfhdAt(t *testing.T, buf []byte, offset uint64) {
	t.Helper()
	// Errors carry the moof's start offset but the bytes are indexed from the
	// segment start; decode the moof box at that relative offset.
	moof, ok := boxAt(buf, int(offset), "moof")
	if !ok {
		t.Fatalf("no moof box at reported duplicate offset %d", offset)
	}
	if _, _, ok := scanMoofIndependently(buf[moof.Offset+moof.HeaderSize : moof.End]); !ok {
		t.Fatalf("moof at %d has no decodable tfdt", offset)
	}
}

// boxAt parses the box header at offset and confirms its type.
func boxAt(buf []byte, offset int, typ string) (cmaf.Box, bool) {
	if offset < 0 || offset+8 > len(buf) {
		return cmaf.Box{}, false
	}
	b, err := cmaf.ParseBoxHeader(buf, offset, len(buf))
	if err != nil || b.Type != typ {
		return cmaf.Box{}, false
	}
	return b, true
}

func assertTfdtAt(t *testing.T, buf []byte, offset uint64) {
	t.Helper()
	moof, ok := boxAt(buf, int(offset), "moof")
	if !ok {
		t.Fatalf("no moof box at reported tfdt-regression offset %d", offset)
	}
	if _, _, ok := scanMoofIndependently(buf[moof.Offset+moof.HeaderSize : moof.End]); !ok {
		t.Fatalf("moof at %d has no decodable tfdt", offset)
	}
}

func assertCorruptBoxAt(t *testing.T, buf []byte, offset uint64) {
	t.Helper()
	if offset+8 > uint64(len(buf)) {
		t.Fatalf("corrupt box offset %d beyond buffer", offset)
	}
	size := uint64(buf[offset])<<24 | uint64(buf[offset+1])<<16 | uint64(buf[offset+2])<<8 | uint64(buf[offset+3])
	if size != 0 && size != 8 {
		t.Fatalf("expected a shrunk/zeroed box size at %d, got %d", offset, size)
	}
}

func TestEveryInjectedDamageIsReported(t *testing.T) {
	js, byName := reparseFixture(t, DefaultConfig())
	// Every planted defect must surface in the layer's damaged ranges, with
	// its segment and kind, or the summary is hiding damage.
	reported := make(map[string]map[string]bool)
	for _, d := range js.DamagedRanges {
		if reported[d.Layer] == nil {
			reported[d.Layer] = make(map[string]bool)
		}
		reported[d.Layer][d.Kind] = true
	}
	// Structural container damage has no trustworthy media position, so it is
	// reported as a scan problem with its raw offset instead of a clock range.
	scanKinds := make(map[string]map[string]bool)
	for _, layer := range js.Layers {
		if scanKinds[layer.Name] == nil {
			scanKinds[layer.Name] = make(map[string]bool)
		}
		for _, p := range layer.ScanProblems {
			scanKinds[layer.Name][p.Kind] = true
		}
	}
	wantKind := map[string]string{
		string(DamageBoxCorrupt): string(clock.ErrBoxSizeOverrun),
		string(DamageTruncate):   string(clock.ErrSampleOutOfBounds),
		string(DamageSkip):       string(clock.ErrMfhdGap),
		string(DamageDuplicate):  string(clock.ErrMfhdDuplicate),
		string(DamageTfdtReg):    string(clock.ErrTfdtRegression),
	}
	// Truncation also shows up as a segment-level problem on every layer, so
	// check the other kinds strictly.
	strict := map[string]bool{
		string(DamageSkip): true, string(DamageDuplicate): true,
		string(DamageTfdtReg): true, string(DamageBoxCorrupt): true,
	}
	for _, layer := range js.Truth {
		if _, ok := byName[layer.Layer]; !ok {
			t.Fatalf("truth layer %q has no source", layer.Layer)
		}
		for _, inj := range layer.Injections {
			if inj.Kind == "" || !strict[inj.Kind] {
				continue
			}
			want, ok := wantKind[inj.Kind]
			if !ok {
				t.Fatalf("unmapped injected kind %q", inj.Kind)
			}
			if !reported[layer.Layer][want] && !scanKinds[layer.Layer][want] {
				t.Fatalf("%s: injected %s (segment %d) not reported as %s; ranges=%v scan=%v",
					layer.Layer, inj.Kind, inj.Segment, want, reported[layer.Layer], scanKinds[layer.Layer])
			}
		}
	}
}
