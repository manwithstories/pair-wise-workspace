// Package fixture generates deterministic synthetic repair cases. Every
// byte is derived from a seed; nothing is read from external media.
package fixture

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"

	"avrepair/internal/media"
	"avrepair/internal/recovery"
	"avrepair/internal/segment"
)

// Case directory names.
const (
	CaseNormal  = "normal"
	CaseLossy   = "lossy"
	CaseCorrupt = "corrupt"
)

// SegmentCount is the number of segments per track. Two indexed tracks
// of 48 segments yield exactly 96 seek index entries.
const SegmentCount = 48

// SegmentDurationMS is the nominal duration of one fixture segment.
const SegmentDurationMS = 1000

// TSScale is the 90 kHz clock used by the TS tracks.
const TSScale int64 = 90000

// Fault schedule. Segment indexes are stable across runs.
type faultPlan struct {
	audioOffsetMS int
	wrapFrom      int // first segment whose raw PTS wrapped past 2^33
	dupAt         int // segment index whose first video PTS repeats
	lossAt        map[int]int
	truncateAt    int
}

func planFor(name string) faultPlan {
	p := faultPlan{audioOffsetMS: 12, wrapFrom: -1, dupAt: -1, truncateAt: -1, lossAt: map[int]int{}}
	switch name {
	case CaseLossy:
		// Single-packet losses only: every one is recoverable.
		for i := 1; i < SegmentCount; i += 5 {
			p.lossAt[i] = (i * 7) % 24
		}
		p.wrapFrom = SegmentCount - 4
		p.dupAt = SegmentCount / 3
	case CaseCorrupt:
		p.truncateAt = SegmentCount / 4
	}
	return p
}

// Generate writes all three cases under dir.
func Generate(dir string) error {
	for _, name := range []string{CaseNormal, CaseLossy, CaseCorrupt} {
		if err := generateCase(filepath.Join(dir, name), name); err != nil {
			return fmt.Errorf("fixture: %s: %w", name, err)
		}
	}
	return nil
}

// CaseAudio and CaseVideo select which presentation side each generated
// case uses. The lossy and normal cases use HLS video plus DASH audio so
// both timelines are merged; the corrupt case uses HLS only so exactly
// one damaged segment remains after the fault schedule runs.
const (
	CaseAudio = "audio"
	CaseVideo = "video"
)

func generateCase(dir, name string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := planFor(name)
	vp := p
	vp.truncateAt = -1
	if name == CaseCorrupt {
		vp.lossAt = map[int]int{}
		vp.dupAt = -1
		vp.wrapFrom = -1
	}
	if err := writeHLS(dir, vp, ""); err != nil {
		return err
	}
	return writeDASH(dir, p)
}

// noise derives deterministic per-segment payload bytes.
func noise(track string, idx int) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("avrepair/%s/%d", track, idx)))
	return sum[:]
}

// ptsOrigin places the timeline just below the 33-bit wrap so a real
// wrap occurs around segment 4. The stored PES value is masked to 33
// bits, so every parser path must unwrap it.
const ptsOrigin = media.Wrap33 - 4*TSScale

// videoPTS is the raw clock value for a segment's first sample.
func videoPTS(segIdx int, p faultPlan) int64 {
	pts := ptsOrigin + int64(segIdx)*TSScale
	if p.dupAt >= 0 && segIdx == p.dupAt+1 {
		pts -= TSScale // repeat the previous boundary timestamp
	}
	return pts
}

// buildCapture assembles an RTP capture with grouped XOR parity. Single
// induced losses become repairable; the multi-loss segment does not.
func buildCapture(track string, segIdx int, video bool, p faultPlan) []byte {
	const packets = 24
	pid, streamID := uint16(0x100), byte(0xE0)
	base := videoPTS(segIdx, p)
	payloads := make([][]byte, packets)
	for i := range payloads {
		sync := video && i == 0
		payloads[i] = segment.BuildTSRecord(uint16(i), pid, streamID, base+int64(i)*3750, sync, noise(track, segIdx)[i])
	}
	lost := map[int]bool{}
	if video {
		if li, ok := p.lossAt[segIdx]; ok {
			lost[li] = true
		}
	}
	out := make([]byte, 0, packets*segment.DataRecordLen+8*segment.ParityRecordLen)
	var group [][]byte
	paritySeq := uint16(packets)
	for i := 0; i < packets; i++ {
		group = append(group, payloads[i])
		if !lost[i] {
			out = append(out, segment.RTPRecord(uint16(i), payloads[i])...)
		}
		if len(group) == recovery.GroupSize {
			base := uint16(i - recovery.GroupSize + 1)
			out = append(out, segment.RTPParity(paritySeq, base, recovery.BuildParity(base, group))...)
			paritySeq++
			group = group[:0]
		}
	}
	return out
}

// writeHLS writes the master and per-track playlists plus TS captures.
func writeHLS(dir string, p faultPlan, caseName string) error {
	sub := filepath.Join(dir, "hls")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return err
	}
	pl, err := m3u8.NewMediaPlaylist(0, SegmentCount+2)
	if err != nil {
		return err
	}
	for i := 0; i < SegmentCount; i++ {
		name := fmt.Sprintf("video-%05d.ts", i)
		if err := os.WriteFile(filepath.Join(sub, name), buildCapture("video", i, true, p), 0o644); err != nil {
			return err
		}
		seg := m3u8.GetSegment()
		seg.URI = "hls/" + name
		seg.Duration = float64(SegmentDurationMS) / 1000
		if err := pl.AppendSegment(seg); err != nil {
			return err
		}
		if i == SegmentCount/2 {
			_ = pl.SetDiscontinuity()
		}
	}
	pl.Close()
	if err := os.WriteFile(filepath.Join(dir, "video.m3u8"), pl.Encode().Bytes(), 0o644); err != nil {
		return err
	}
	master := m3u8.NewMasterPlaylist()
	master.Append("video.m3u8", nil, m3u8.VariantParams{Bandwidth: 800000, Codecs: "avc1.4d401e"})
	_ = caseName
	return os.WriteFile(filepath.Join(dir, "index.m3u8"), master.Encode().Bytes(), 0o644)
}

// buildMP4 builds a minimal fragmented MP4 segment. The corrupt case
// truncates the moof of one segment, which the parser must reject
// without aborting the whole timeline.
func buildMP4(track string, segIdx int, p faultPlan) []byte {
	const timescale int64 = 90000
	base := ptsOrigin + int64(segIdx)*timescale + int64(p.audioOffsetMS)*90
	moov := segment.BuildMoov(1, uint32(timescale))
	moof := segment.BuildMoof(1, base, uint32(timescale/10), 10, true)
	mdat := segment.MP4Box("mdat", noise(track, segIdx))
	data := append(append(append([]byte(nil), moov...), moof...), mdat...)
	if p.truncateAt >= 0 && segIdx == p.truncateAt && track == "audio-1" {
		// Cut inside the moov header so no usable box remains.
		return data[:40]
	}
	return data
}

// writeDASH writes a presentation MPD and its fMP4 segments.
func writeDASH(dir string, p faultPlan) error {
	sub := filepath.Join(dir, "dash")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return err
	}
	for i := 0; i < SegmentCount; i++ {
		name := fmt.Sprintf("audio-1-%05d.m4s", i)
		if err := os.WriteFile(filepath.Join(sub, name), buildMP4("audio-1", i, p), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "presentation.mpd"), []byte(mpdText()), 0o644)
}

func mpdText() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT48S">
  <Period id="p0" start="PT0S">
    <AdaptationSet id="2" contentType="audio" mimeType="audio/mp4">
      <Representation id="audio-1" codecs="mp4a.40.2" bandwidth="64000">
        <SegmentTemplate timescale="90000" media="dash/audio-1-$Number%05d$.m4s" startNumber="0">
          <SegmentTimeline><S t="0" d="90000" r="47"/></SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`
}
