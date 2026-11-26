// Package fixture generates the self-contained sample corpus used by the
// scanner and the tests: two DASH representations, an HLS playlist with
// two discontinuities, and twenty deterministic fMP4 segments. All bytes
// are derived from fixed seeds, so generation is reproducible and never
// references external files.
package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Corpus layout constants.
const (
	Timescale      = 90000
	SamplesPerSeg  = 12
	SampleDuration = 3750 // 1/24 s at 90 kHz
	SegDuration    = SamplesPerSeg * SampleDuration
	HLSSegments    = 10
	DashSegments   = 5 // per representation
	DashRepA       = "a"
	DashRepB       = "b"
	// TotalSegments is the number of fMP4 segments in a full corpus.
	TotalSegments = HLSSegments + 2*DashSegments
)

// Fault selects a defect injected into the generated corpus.
type Fault string

const (
	FaultNone            Fault = ""
	FaultTruncatedBox    Fault = "truncated-box"
	FaultMissingSegment  Fault = "missing-segment"
	FaultInvalidDuration Fault = "invalid-duration"
	FaultTfdtRollback    Fault = "tfdt-rollback"
	FaultDiscBoundary    Fault = "discontinuity-boundary"
)

// ParseFault validates a --fault flag value.
func ParseFault(s string) (Fault, error) {
	switch Fault(s) {
	case FaultNone, FaultTruncatedBox, FaultMissingSegment,
		FaultInvalidDuration, FaultTfdtRollback, FaultDiscBoundary:
		return Fault(s), nil
	}
	return FaultNone, fmt.Errorf("unknown fault %q (want one of: truncated-box, missing-segment, invalid-duration, tfdt-rollback, discontinuity-boundary)", s)
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// box returns a size-prefixed MP4 box.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

// fullBox prepends the version/flags word to a box payload.
func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(head, payload...))
}

// sampleBytes builds one deterministic AVCC sample: a 4-byte length
// prefix followed by a single NAL unit whose payload is derived from a
// SHA-256 chain over the seed.
func sampleBytes(seed string, sampleIdx int, idr bool) []byte {
	nalHeader := byte(0x65) // AVC IDR slice (nal_unit_type 5)
	if !idr {
		nalHeader = 0x41 // non-IDR slice (nal_unit_type 1)
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/sample-%d", seed, sampleIdx)))
	payloadLen := 24 + (sampleIdx%3)*8
	payload := []byte{nalHeader}
	for len(payload) < payloadLen {
		payload = append(payload, h[:]...)
		h = sha256.Sum256(h[:])
	}
	payload = payload[:payloadLen]
	out := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(payload)))
	copy(out[4:], payload)
	return out
}

// segParams describes one generated media segment.
type segParams struct {
	seq  uint32 // mfhd sequence number
	tfdt uint64 // baseMediaDecodeTime in Timescale units
	seed string
}

// buildSegment renders one fMP4 segment: styp, moof (mfhd+traf), mdat.
// Every sixth sample is an IDR, so each segment carries two keyframes.
func buildSegment(p segParams) []byte {
	samples := make([][]byte, SamplesPerSeg)
	for i := range samples {
		samples[i] = sampleBytes(p.seed, i, i%6 == 0)
	}

	mfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdPayload, p.seq)
	mfhd := fullBox("mfhd", 0, 0, mfhdPayload)

	tfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhdPayload, 1)        // track_ID
	tfhd := fullBox("tfhd", 0, 0x020000, tfhdPayload) // default-base-is-moof

	tfdtPayload := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtPayload, p.tfdt)
	tfdt := fullBox("tfdt", 1, 0, tfdtPayload)

	// trun: data-offset + per-sample duration/size/flags.
	trunBody := make([]byte, 0, 8+SamplesPerSeg*12)
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(SamplesPerSeg))
	trunBody = append(trunBody, tmp[:]...)
	trunBody = append(trunBody, 0, 0, 0, 0) // data-offset, patched below
	for i, s := range samples {
		flags := uint32(0x00010000) // sample_is_non_sync_sample
		if i%6 == 0 {
			flags = 0x02000000 // depends_on=2 (intra), sync bit clear
		}
		binary.BigEndian.PutUint32(tmp[:], SampleDuration)
		trunBody = append(trunBody, tmp[:]...)
		binary.BigEndian.PutUint32(tmp[:], uint32(len(s)))
		trunBody = append(trunBody, tmp[:]...)
		binary.BigEndian.PutUint32(tmp[:], flags)
		trunBody = append(trunBody, tmp[:]...)
	}
	trun := fullBox("trun", 0, 0x000701, trunBody)

	traf := box("traf", concat(tfhd, tfdt, trun))
	moof := box("moof", concat(mfhd, traf))

	// Patch the trun data-offset: moof size + mdat header size, relative
	// to the start of moof (default-base-is-moof).
	dataOffsetField := 8 + len(mfhd) + 8 + len(tfhd) + len(tfdt) + 12 + 4
	binary.BigEndian.PutUint32(moof[dataOffsetField:], uint32(len(moof)+8))

	var mdatPayload []byte
	for _, s := range samples {
		mdatPayload = append(mdatPayload, s...)
	}
	mdat := box("mdat", mdatPayload)

	styp := box("styp", []byte("msdh\x00\x00\x00\x00msdhmsix"))
	return concat(styp, moof, mdat)
}

// pdtBase is the program-date-time anchor for the HLS playlist.
var pdtBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// hlsDiscontinuities lists the segment indices that open a new
// discontinuity sequence (two discontinuities in a clean corpus).
var hlsDiscontinuities = map[int]bool{3: true, 7: true}

func hlsMasterPlaylist() string {
	return "#EXTM3U\n" +
		"#EXT-X-VERSION:7\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS=\"avc1.4d401f\",RESOLUTION=1280x720\n" +
		"media.m3u8\n"
}

func hlsMediaPlaylist(fault Fault) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	half := time.Duration(SegDuration) * time.Second / time.Duration(Timescale)
	for i := 0; i < HLSSegments; i++ {
		if hlsDiscontinuities[i] {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		pdt := pdtBase.Add(time.Duration(i) * half)
		if fault == FaultDiscBoundary && i == 7 {
			pdt = pdtBase.Add(-time.Hour) // rollback across the boundary
		}
		b.WriteString("#EXT-X-PROGRAM-DATE-TIME:" + pdt.Format("2006-01-02T15:04:05.000Z") + "\n")
		dur := "0.500000"
		if fault == FaultInvalidDuration && i == 2 {
			dur = "-0.500000"
		}
		fmt.Fprintf(&b, "#EXTINF:%s,\nseg%d.m4s\n", dur, i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func dashMPD() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT2.5S">
  <Period id="p0" duration="PT2.5S">
    <AdaptationSet id="1" mimeType="video/mp4" codecs="avc1.4d401f">
      <Representation id="a" bandwidth="800000" width="1280" height="720">
        <SegmentTemplate timescale="90000" presentationTimeOffset="0" startNumber="0" media="a/seg$Number$.m4s">
          <SegmentTimeline>
            <S t="0" d="45000" r="4"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
      <Representation id="b" bandwidth="400000" width="1280" height="720">
        <SegmentTemplate timescale="90000" presentationTimeOffset="90000" startNumber="0" media="b/seg$Number$.m4s">
          <SegmentTimeline>
            <S t="90000" d="45000" r="4"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`
}

// Generate writes the corpus under out, creating directories as needed.
// Output is deterministic: the same fault always yields the same bytes.
func Generate(out string, fault Fault) error {
	writeFile := func(rel string, data []byte) error {
		p := filepath.Join(out, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, data, 0o644)
	}

	for i := 0; i < HLSSegments; i++ {
		if fault == FaultMissingSegment && i == 5 {
			continue
		}
		data := buildSegment(segParams{
			seq:  uint32(i + 1),
			tfdt: uint64(i * SegDuration),
			seed: fmt.Sprintf("hls/seg%d", i),
		})
		if fault == FaultTruncatedBox && i == 4 {
			data = data[:len(data)-32] // cut the mdat payload short
		}
		if err := writeFile(fmt.Sprintf("hls/seg%d.m4s", i), data); err != nil {
			return err
		}
	}

	reps := []struct {
		name string
		pto  uint64
	}{{DashRepA, 0}, {DashRepB, 90000}}
	for _, r := range reps {
		for i := 0; i < DashSegments; i++ {
			tfdt := r.pto + uint64(i*SegDuration)
			if fault == FaultTfdtRollback && r.name == DashRepA && i == 2 {
				tfdt = 0 // rolls back below the previous segment
			}
			data := buildSegment(segParams{
				seq:  uint32(100 + i),
				tfdt: tfdt,
				seed: fmt.Sprintf("dash/%s/seg%d", r.name, i),
			})
			if err := writeFile(fmt.Sprintf("dash/%s/seg%d.m4s", r.name, i), data); err != nil {
				return err
			}
		}
	}

	if err := writeFile("hls/master.m3u8", []byte(hlsMasterPlaylist())); err != nil {
		return err
	}
	if err := writeFile("hls/media.m3u8", []byte(hlsMediaPlaylist(fault))); err != nil {
		return err
	}
	return writeFile("dash/manifest.mpd", []byte(dashMPD()))
}
