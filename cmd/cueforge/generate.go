package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"cueforge/internal/model"
)

// trackFile describes one generated track fixture.
type trackFile struct {
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Track int    `json:"track"`
}

// fixtures describes the generated demo data set.
type fixtures struct {
	Dir         string      `json:"dir"`
	Tracks      []trackFile `json:"tracks"`
	BadPath     string      `json:"bad_path"`
	BadOffset   int64       `json:"bad_offset"`
	TotalEvents int         `json:"total_events"`
	TotalTimeMs int64       `json:"total_time_ms"`
}

func (f *fixtures) manifestPath() string { return filepath.Join(f.Dir, "manifest.json") }

func (f *fixtures) saveManifest() error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(f.manifestPath(), b, 0o644)
}

func loadFixtures(dir string) (*fixtures, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var fx fixtures
	if err := json.Unmarshal(b, &fx); err != nil {
		return nil, err
	}
	return &fx, nil
}

func msTimestamp(ms int64) string {
	h := ms / 3600000
	m := (ms / 60000) % 60
	s := (ms / 1000) % 60
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms%1000)
}

// eventSpec plans one event on one track.
type eventSpec struct {
	startMs int64
	durMs   int64
	label   string
}

// planEvents distributes total events across 5 tracks deterministically.
func planEvents(total int, rng *rand.Rand) (perTrack [][]eventSpec, totalMs int64) {
	const nTracks = 5
	perTrack = make([][]eventSpec, nTracks)
	base := total / nTracks
	var maxEnd int64
	for t := 0; t < nTracks; t++ {
		n := base
		if t == nTracks-1 {
			n += total % nTracks
		}
		for i := 0; i < n; i++ {
			start := int64(i)*32 + int64(t%2)
			dur := int64(24)
			label := fmt.Sprintf("t%d-e%04d-%08x", t, i, rng.Uint32())
			perTrack[t] = append(perTrack[t], eventSpec{start, dur, label})
			if end := start + dur; end > maxEnd {
				maxEnd = end
			}
		}
	}
	return perTrack, maxEnd
}

func writeVTT(path string, evs []eventSpec) error {
	var b bytes.Buffer
	b.WriteString("WEBVTT\n\n")
	for _, e := range evs {
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n",
			msTimestamp(e.startMs), msTimestamp(e.startMs+e.durMs), "cue-"+e.label)
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

func writeChapters(path string, evs []eventSpec) error {
	var b bytes.Buffer
	for i, e := range evs {
		fmt.Fprintf(&b, `{"id":%d,"start_ms":%d,"end_ms":%d,"title":"ch-%s"}`+"\n",
			i, e.startMs, e.startMs+e.durMs, e.label)
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

func putSyncsafe(b []byte, v int) {
	b[0] = byte(v >> 21 & 0x7f)
	b[1] = byte(v >> 14 & 0x7f)
	b[2] = byte(v >> 7 & 0x7f)
	b[3] = byte(v & 0x7f)
}

func writeID3(path string, evs []eventSpec) error {
	var frames bytes.Buffer
	for _, e := range evs {
		payload := fmt.Sprintf("start=%d/1000;dur=%d/1000;label=id3-%s",
			e.startMs, e.durMs, e.label)
		frames.WriteString("TXXX")
		var sz [4]byte
		putSyncsafe(sz[:], len(payload))
		frames.Write(sz[:])
		frames.Write([]byte{0, 0})
		frames.WriteString(payload)
	}
	var out bytes.Buffer
	out.WriteString("ID3")
	out.Write([]byte{4, 0, 0})
	var tag [4]byte
	putSyncsafe(tag[:], frames.Len())
	out.Write(tag[:])
	out.Write(frames.Bytes())
	return os.WriteFile(path, out.Bytes(), 0o644)
}

func emsgBox(startMs, durMs int64, id uint32, value string) []byte {
	var b bytes.Buffer
	body := new(bytes.Buffer)
	body.Write([]byte{1, 0, 0, 0}) // version 1, flags
	binary.Write(body, binary.BigEndian, uint32(1000))
	binary.Write(body, binary.BigEndian, uint64(startMs))
	binary.Write(body, binary.BigEndian, uint32(durMs))
	binary.Write(body, binary.BigEndian, id)
	body.WriteString("urn:cueforge:demo")
	body.WriteByte(0)
	body.WriteString(value)
	body.WriteByte(0)
	body.WriteString("md")
	binary.Write(&b, binary.BigEndian, uint32(8+body.Len()))
	b.WriteString("emsg")
	b.Write(body.Bytes())
	return b.Bytes()
}

func writeEMSG(path string, evs []eventSpec) error {
	var b bytes.Buffer
	for i, e := range evs {
		b.Write(emsgBox(e.startMs, e.durMs, uint32(i), "emsg-"+e.label))
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

var seiUUID = []byte{0x43, 0x55, 0x45, 0x46, 0x4f, 0x52, 0x47, 0x45,
	0x2d, 0x53, 0x45, 0x49, 0x2d, 0x30, 0x30, 0x31}

func writeSEI(path string, evs []eventSpec) error {
	var b bytes.Buffer
	for _, e := range evs {
		text := fmt.Sprintf("start=%d/1000;dur=%d/1000;label=sei-%s",
			e.startMs, e.durMs, e.label)
		payload := append(append([]byte{}, seiUUID...), text...)
		b.Write([]byte{0, 0, 1, 0x06}) // start code + SEI NAL header
		b.WriteByte(5)                 // payloadType: user_data_unregistered
		n := len(payload)
		for n >= 255 {
			b.WriteByte(0xff)
			n -= 255
		}
		b.WriteByte(byte(n))
		b.Write(payload)
		b.WriteByte(0x80) // rbsp_trailing_bits
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// writeCorruptEMSG writes valid emsg boxes, pads with a free box so a
// truncated emsg box starts exactly at badOffset.
func writeCorruptEMSG(path string, badOffset int64, rng *rand.Rand) error {
	var b bytes.Buffer
	for i := 0; i < 8; i++ {
		b.Write(emsgBox(int64(i)*32, 24, uint32(i),
			fmt.Sprintf("emsg-bad-%04d-%08x", i, rng.Uint32())))
	}
	pad := badOffset - int64(b.Len())
	if pad < 8 {
		return fmt.Errorf("cannot align corrupt fixture: need %d pad bytes", pad)
	}
	binary.Write(&b, binary.BigEndian, uint32(pad))
	b.WriteString("free")
	b.Write(make([]byte, pad-8))
	// Truncated emsg box: declares 512 bytes, file ends after 12.
	binary.Write(&b, binary.BigEndian, uint32(512))
	b.WriteString("emsg")
	b.Write([]byte{1, 0, 0, 0})
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// generate creates the full demo data set under dir.
func generate(dir string, seed int64, events int) (*fixtures, error) {
	if events <= 0 || events > model.MaxEvents {
		return nil, fmt.Errorf("event count %d out of range (1..%d)", events, model.MaxEvents)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(seed))
	perTrack, totalMs := planEvents(events, rng)
	kinds := []string{"vtt", "chapters", "id3", "emsg", "sei"}
	names := []string{"track0.vtt", "track1.jsonl", "track2.id3", "track3.m4s", "track4.h264"}
	writers := []func(string, []eventSpec) error{writeVTT, writeChapters, writeID3, writeEMSG, writeSEI}
	fx := &fixtures{Dir: dir, BadOffset: 8192, TotalEvents: events, TotalTimeMs: totalMs}
	for t := range kinds {
		p := filepath.Join(dir, names[t])
		if err := writers[t](p, perTrack[t]); err != nil {
			return nil, err
		}
		fx.Tracks = append(fx.Tracks, trackFile{Path: p, Kind: kinds[t], Track: t})
	}
	fx.BadPath = filepath.Join(dir, "corrupt.m4s")
	if err := writeCorruptEMSG(fx.BadPath, fx.BadOffset, rng); err != nil {
		return nil, err
	}
	if err := fx.saveManifest(); err != nil {
		return nil, err
	}
	return fx, nil
}
