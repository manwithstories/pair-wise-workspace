// Package pipeline generates self-contained media fixtures, wires every
// processing stage into the DAG and produces the terminal verification report.
package pipeline

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"mediad/internal/model"
)

// Sample rate shared by every generated PCM payload (8 kHz mono int16 LE).
const sampleRate = 8000

// Segment duration in milliseconds for every generated fragment.
const segMS = 2000

// Layout describes the generated on-disk fixture tree.
type Layout struct {
	Root      string
	Manifests map[model.TrackRole]string
	Segments  map[model.TrackRole][]string
	// Corrupt lists deliberately damaged fragments with the expected first
	// bad byte offset (file-relative), used by tests to assert ±1 accuracy.
	Corrupt []CorruptSpec
}

// CorruptSpec is one injected structural defect.
type CorruptSpec struct {
	Track        model.TrackRole
	Index        int
	Path         string
	ExpectOffset int64
	Kind         string
}

// Generate writes a complete fixture tree under root. When healthy is false,
// two recoverable defects are injected: a tail truncation and a box whose
// declared length exceeds the file.
func Generate(root string, healthy bool) (*Layout, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	l := &Layout{
		Root:      root,
		Manifests: map[model.TrackRole]string{},
		Segments:  map[model.TrackRole][]string{},
	}
	// Deterministic PCM: 2 s per segment, 400 ms of silence in the middle.
	basePCM := genPCM(segMS, sampleRate, 6000, 9200)
	baseFrag := buildFragment(basePCM, false)

	if err := genStudioHLS(root, l, baseFrag, healthy); err != nil {
		return nil, err
	}
	if err := genFieldDASH(root, l, baseFrag, basePCM); err != nil {
		return nil, err
	}
	if err := genVoiceHLS(root, l, basePCM, healthy); err != nil {
		return nil, err
	}
	return l, nil
}

func genStudioHLS(root string, l *Layout, baseFrag []byte, healthy bool) error {
	dir := filepath.Join(root, "studio")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	const n = 6
	var names []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("seg-%d.m4s", i)
		if err := os.WriteFile(filepath.Join(dir, name), baseFrag, 0o600); err != nil {
			return err
		}
		names = append(names, name)
	}
	pl := "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n"
	for i := 0; i < n; i++ {
		pl += "#EXTINF:2.0,\n" + names[i] + "\n"
	}
	pl += "#EXT-X-ENDLIST\n"
	plist := filepath.Join(dir, "playlist.m3u8")
	if err := os.WriteFile(plist, []byte(pl), 0o600); err != nil {
		return err
	}
	l.Manifests[model.TrackStudio] = "studio/playlist.m3u8"
	l.Segments[model.TrackStudio] = names
	if !healthy {
		// Tail truncation mid-header of the trailing free box. The free box
		// (16 bytes) starts at len-16; keeping only 3 dangling header bytes
		// makes the walker hit ErrUnexpectedEOF and report the first
		// unreadable byte (pos + bytes-read), i.e. exactly the cut point.
		target := filepath.Join(dir, names[4])
		freeHeader := len(baseFrag) - 16 // 8-byte header + 8-byte "padding!"
		cut := freeHeader + 3
		if err := os.WriteFile(target, baseFrag[:cut], 0o600); err != nil {
			return err
		}
		l.Corrupt = append(l.Corrupt, CorruptSpec{
			Track: model.TrackStudio, Index: 4, Path: target,
			ExpectOffset: int64(cut), Kind: "truncated",
		})
	}
	return nil
}

func genFieldDASH(root string, l *Layout, baseFrag []byte, basePCM []int16) error {
	dir := filepath.Join(root, "field")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	const n = 6
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("f-%d.m4s", i)
		if err := os.WriteFile(filepath.Join(dir, name), baseFrag, 0o600); err != nil {
			return err
		}
		l.Segments[model.TrackField] = append(l.Segments[model.TrackField], name)
	}
	// Segment 0 of the studio track shares identical bytes -> duplicate.
	mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet id="field">
<SegmentTemplate timescale="1000" media="f-$Number$.m4s" startNumber="1">
<SegmentTimeline><S t="0" d="2000" r="5"/></SegmentTimeline>
</SegmentTemplate></AdaptationSet></Period></MPD>`
	if err := os.WriteFile(filepath.Join(dir, "manifest.mpd"), []byte(mpd), 0o600); err != nil {
		return err
	}
	l.Manifests[model.TrackField] = "field/manifest.mpd"
	return nil
}

func genVoiceHLS(root string, l *Layout, basePCM []int16, healthy bool) error {
	dir := filepath.Join(root, "voice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	const n = 4
	var names []string
	// Distinct tone (fixed offset, silence window preserved) so the voice
	// track contributes unique content alongside the studio/field duplicate.
	voicePCM := make([]int16, len(basePCM))
	for i, v := range basePCM {
		if v != 0 {
			voicePCM[i] = v + 100
		}
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("v-%d.m4s", i)
		frag := buildFragment(voicePCM, false)
		if err := os.WriteFile(filepath.Join(dir, name), frag, 0o600); err != nil {
			return err
		}
		names = append(names, name)
	}
	pl := "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:2\n"
	for i := 0; i < n; i++ {
		pl += "#EXTINF:2.0,\n" + names[i] + "\n"
	}
	pl += "#EXT-X-ENDLIST\n"
	plist := filepath.Join(dir, "playlist.m3u8")
	if err := os.WriteFile(plist, []byte(pl), 0o600); err != nil {
		return err
	}
	l.Manifests[model.TrackVoice] = "voice/playlist.m3u8"
	l.Segments[model.TrackVoice] = names
	if !healthy {
		// Inflate the mdat declared length beyond the file: the bad offset is
		// the mdat box header itself.
		target := filepath.Join(dir, names[2])
		data, err := os.ReadFile(target)
		if err != nil {
			return err
		}
		mdatOff := locateBox(data, "mdat")
		if mdatOff < 0 {
			return fmt.Errorf("fixture: mdat box not found in %s", target)
		}
		binary.BigEndian.PutUint32(data[mdatOff:mdatOff+4], uint32(len(data))+10_000)
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return err
		}
		l.Corrupt = append(l.Corrupt, CorruptSpec{
			Track: model.TrackVoice, Index: 2, Path: target,
			ExpectOffset: mdatOff, Kind: "box-out-of-range",
		})
	}
	return nil
}

// genPCM builds deterministic loud samples with a silent [from,to) window.
func genPCM(durationMS, rate, silentFrom, silentTo int) []int16 {
	n := rate * durationMS / 1000
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		if i >= silentFrom && i < silentTo {
			out[i] = 0
			continue
		}
		// Deterministic pseudo-tone bounded well above the silence threshold.
		out[i] = int16(300 + (i*37)%400)
	}
	return out
}

func buildFragment(pcm []int16, nonSync bool) []byte {
	payload := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(payload[i*2:], uint16(v))
	}
	var flags uint32
	var defaultFlags *uint32
	if nonSync {
		flags = 0x000020
		df := uint32(0x01010000)
		defaultFlags = &df
	}
	tfhd := boxBytes("tfhd", tfhdPayload(1, flags, defaultFlags))
	traf := boxBytes("traf", tfhd)
	moof := boxBytes("moof", traf)
	var b []byte
	b = append(b, boxBytes("ftyp", []byte("iso5dash"))...)
	b = append(b, moof...)
	b = append(b, boxBytes("mdat", payload)...)
	// Trailing free box gives truncation a real box boundary to cut into.
	b = append(b, boxBytes("free", []byte("padding!"))...)
	return b
}

func boxBytes(btype string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:8], btype)
	copy(out[8:], payload)
	return out
}

func tfhdPayload(trackID uint32, flags uint32, defaultFlags *uint32) []byte {
	p := []byte{0, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	p = append(p, byte(trackID>>24), byte(trackID>>16), byte(trackID>>8), byte(trackID))
	if defaultFlags != nil {
		p = binary.BigEndian.AppendUint32(p, *defaultFlags)
	}
	return p
}

func locateBox(data []byte, want string) int64 {
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		if string(data[off+4:off+8]) == want {
			return int64(off)
		}
		if size < 8 {
			return -1
		}
		off += size
	}
	return -1
}
