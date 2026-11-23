package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"mediasim/internal/clock"
)

// Build writes a deterministic MP4 for the spec and returns the bytes and
// the manifest describing what the file should contain.
func Build(spec Spec) ([]byte, *Manifest, error) {
	if spec.Cam0 < 0 || spec.Cam1 < 0 || spec.Audio < 0 {
		return nil, nil, fmt.Errorf("fixture: negative sample count")
	}
	if spec.Cam0 > 600 || spec.Cam1 > 600 || spec.Audio > 1200 {
		return nil, nil, fmt.Errorf("fixture: sample counts exceed limits")
	}
	man := manifestFor(spec)
	m := &builder{spec: spec, man: man}
	if err := m.build(); err != nil {
		return nil, nil, err
	}
	return m.finish(), man, nil
}

func manifestFor(spec Spec) *Manifest {
	man := &Manifest{
		Name: spec.Name, Seed: spec.Seed,
		TickRate: clock.MustRational(TickNum, TickDen),
		FPS:      clock.MustRational(FPSNum, FPSDen),
		Fault:    spec.Fault,
	}
	if spec.Cam0 > 0 {
		man.Tracks = append(man.Tracks, TrackSpec{
			ID: TrackCam0, Kind: "video", Source: SourceCam0, Count: spec.Cam0,
			SampleSize: VideoBytes, FrameTicks: VideoFrameTicks,
			Timescale: VideoTimescale, Delta: VideoDelta, KeyframeEvery: KeyframeEvery,
			Codec: "avc1",
		})
	}
	if spec.Cam1 > 0 {
		man.Tracks = append(man.Tracks, TrackSpec{
			ID: TrackCam1, Kind: "video", Source: SourceCam1, Count: spec.Cam1,
			SampleSize: VideoBytes, FrameTicks: VideoFrameTicks,
			Timescale: VideoTimescale, Delta: VideoDelta, KeyframeEvery: KeyframeEvery,
			Codec: "avc1",
		})
	}
	if spec.Audio > 0 {
		man.Tracks = append(man.Tracks, TrackSpec{
			ID: TrackVoice, Kind: "audio", Source: SourceVoice, Count: spec.Audio,
			SampleSize: AudioBytes, FrameTicks: AudioChunkTicks,
			Timescale: AudioTimescale, Delta: AudioDelta, Codec: "mp4a",
		})
	}
	man.TotalTicks = TotalTicks
	return man
}

func (b *builder) build() error {
	// Compute mdat-relative offsets first so stco can be written in one pass.
	var layouts []trackLayout
	cursor := int64(0)
	for _, ts := range b.man.Tracks {
		lay := trackLayout{spec: ts}
		for i := 0; i < ts.Count; i++ {
			lay.offsets = append(lay.offsets, uint32(cursor))
			lay.sizes = append(lay.sizes, uint32(ts.SampleSize))
			if ts.Kind == "video" && isKeyframe(i, ts.KeyframeEvery) {
				lay.syncs = append(lay.syncs, uint32(i+1))
			}
			cursor += int64(ts.SampleSize)
		}
		lay.mdatLen = int(cursor)
		layouts = append(layouts, lay)
	}
	b.layout = layouts

	// mdat payload, sample by sample.
	for _, lay := range layouts {
		for i := 0; i < lay.spec.Count; i++ {
			sync := lay.spec.Kind == "audio" || isKeyframe(i, lay.spec.KeyframeEvery)
			b.mdat.Write(samplePayload(b.spec.Seed, lay.spec.Source, i, lay.spec.SampleSize, sync))
		}
	}

	moov := b.buildMoovAt(0)
	// ftyp + moov + mdat
	ftyp := b.buildFtyp()

	// Relative offsets in mdat were computed from 0; rebuild stco against the
	// absolute mdat payload start.
	mdatPayloadStart := int64(len(ftyp)) + int64(len(moov)) + 8
	moov = b.buildMoovAt(mdatPayloadStart)

	b.buf.Reset()
	b.buf.Write(ftyp)
	b.buf.Write(moov)
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(8+b.mdat.Len()))
	copy(hdr[4:], "mdat")
	b.buf.Write(hdr[:])
	b.buf.Write(b.mdat.Bytes())

	if b.spec.Fault.Corrupt != nil {
		b.applyCorruption()
	}
	return nil
}

func (b *builder) finish() []byte {
	data := append([]byte(nil), b.buf.Bytes()...)
	if b.spec.Fault.TruncateKeepBytes > 0 && int64(len(data)) > b.spec.Fault.TruncateKeepBytes {
		data = data[:b.spec.Fault.TruncateKeepBytes]
	}
	return data
}

func (b *builder) applyCorruption() {
	out := b.buf.Bytes()
	base := int64(len(b.buildFtyp())) + int64(len(b.buildMoovAt(0)))
	base += 8 // mdat header
	for _, ref := range b.spec.Fault.Corrupt {
		for _, lay := range b.layout {
			if lay.spec.Source != ref.Source || ref.Index >= lay.spec.Count {
				continue
			}
			at := base + int64(lay.offsets[ref.Index])
			copy(out[at:at+4], []byte{0xde, 0xad, 0xbe, 0xef})
		}
	}
}

func (b *builder) buildFtyp() []byte {
	payload := make([]byte, 0, 4+4+16)
	payload = append(payload, []byte("isom")...)
	payload = binary.BigEndian.AppendUint32(payload, 512)
	payload = append(payload, []byte("isom")...)
	payload = append(payload, []byte("iso2")...)
	payload = append(payload, []byte("avc1")...)
	payload = append(payload, []byte("mp41")...)
	payload = append(payload, []byte("msmp")...)
	return box("ftyp", payload)
}

// box wraps a payload with a standard 8-byte header.

// box wraps a payload with a standard 8-byte header.

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

// containerBox wraps children.
func containerBox(typ string, children ...[]byte) []byte {
	var payload []byte
	for _, c := range children {
		payload = append(payload, c...)
	}
	return box(typ, payload)
}

// fullbox wraps a payload with version+flags.
func fullbox(typ string, version byte, flags uint32, payload []byte) []byte {
	pre := make([]byte, 4)
	pre[0] = version
	pre[1] = byte(flags >> 16)
	pre[2] = byte(flags >> 8)
	pre[3] = byte(flags)
	return box(typ, append(pre, payload...))
}

// Ensure writes the named preset into dir if it is not already present and
// returns the file path plus its manifest.
func Ensure(dir, name string) (string, *Manifest, error) {
	spec, err := PresetSpec(name)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, spec.Name+".mp4")
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		// Reuse the existing file, but rebuild the manifest deterministically.
		_, man, err := Build(spec)
		return path, man, err
	}
	data, man, err := Build(spec)
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", nil, err
	}
	return path, man, nil
}
