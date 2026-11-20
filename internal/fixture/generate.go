// Package fixture deterministically generates the local-only sample media the
// whole pipeline operates on: four JSONL frame tracks totalling roughly ten
// thousand RTP frames, a project manifest declaring clocks/dependencies/policy
// and a controlled set of defects (checksum corruption, timestamp rollover,
// duplicate/backwards/missing frames and color anomalies). No network access
// is involved; every byte is derived from a seed so reruns are identical.
package fixture

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mediamend/mediamend/internal/audio"
	"github.com/mediamend/mediamend/internal/graph"
	"github.com/pion/rtp"
)

// DefaultFramesPerTrack gives 4 tracks * 2500 = 10000 frames.
const DefaultFramesPerTrack = 2500

// TrackSpec is the project-level declaration of one track.
type TrackSpec struct {
	ID        string     `json:"id"`
	Kind      graph.Kind `json:"kind"`
	Rate      uint64     `json:"rate"`
	FrameStep uint64     `json:"frame_step"`
	Frames    uint64     `json:"frames"`
	File      string     `json:"file"`
	Layout    string     `json:"layout,omitempty"`
	Color     *ColorDecl `json:"color,omitempty"`
}

// ColorDecl is the project color declaration inherited by missing frames.
type ColorDecl struct {
	Primaries uint16 `json:"primaries"`
	Transfer  uint16 `json:"transfer"`
	Matrix    uint16 `json:"matrix"`
	FullRange bool   `json:"full_range"`
}

// Project is the on-disk manifest tying inputs to clocks and policy.
type Project struct {
	Name        string                `json:"name"`
	Tracks      []TrackSpec           `json:"tracks"`
	AudioPolicy audio.Policy          `json:"audio_policy"`
	Colors      map[string]*ColorDecl `json:"colors"`
	KeyEvery    uint32                `json:"key_every"`
}

// Options controls defect injection. Zero values produce a clean dataset.
type Options struct {
	Root             string
	Seed             int64
	FramesPerTrack   uint64
	CorruptEvery     uint64 // every Nth frame gets a bad SHA-256 (0 disables)
	InjectRollover   bool
	InjectDuplicate  bool
	InjectBackwards  bool
	InjectMissing    bool
	InjectColorFault bool
}

// record is the ingest JSONL wire record.
type record struct {
	TrackID  string     `json:"track_id"`
	Seq      uint64     `json:"frame_seq"`
	DeviceTS uint64     `json:"device_ts"`
	Rate     uint64     `json:"rate"`
	RTPB64   string     `json:"rtp_b64"`
	SHA      string     `json:"sha256"`
	Color    *ColorDecl `json:"color,omitempty"`
}

// CorruptSeq returns the sequence numbers that receive a bad checksum for the
// given CorruptEvery, so tests can assert location precision without guessing.
func CorruptSeq(total, every uint64) []uint64 {
	if every == 0 {
		return nil
	}
	var out []uint64
	for s := every - 1; s < total; s += every {
		out = append(out, s)
	}
	return out
}

// rtpPacket builds a deterministic RTP packet for one frame.
func rtpPacket(trackSSRC uint32, seq uint16, ts uint32, frame uint64) []byte {
	payload := make([]byte, 24)
	for i := range payload {
		payload[i] = byte(frame*31 + uint64(i)*7)
	}
	p := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq,
			Timestamp: ts, SSRC: trackSSRC, Marker: true,
		},
		Payload: payload,
	}
	b, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

func writeLine(f *os.File, rec record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func trackSSRC(id string) uint32 {
	h := sha256.Sum256([]byte(id))
	return uint32(h[0])<<24 | uint32(h[1])<<16 | uint32(h[2])<<8 | uint32(h[3])
}

// defaultProject declares the four canonical tracks and explicit policies.
func defaultProject(root string, perTrack uint64) *Project {
	spec := func(id string, kind graph.Kind, rate, step uint64, layout string) TrackSpec {
		return TrackSpec{
			ID: id, Kind: kind, Rate: rate, FrameStep: step, Frames: perTrack,
			File: filepath.Join(root, "input", id+".jsonl"), Layout: layout,
			Color: &ColorDecl{Primaries: 1, Transfer: 1, Matrix: 1},
		}
	}
	return &Project{
		Name: "multi-track-interview",
		Tracks: []TrackSpec{
			spec("v1", graph.KindVideo, 90000, 3000, ""),
			spec("v2", graph.KindVideo, 90000, 3000, ""),
			spec("a1", graph.KindAudio, 48000, 48, "stereo"),
			spec("a2", graph.KindAudio, 48000, 48, "mono"),
		},
		AudioPolicy: audio.Policy{TargetLayout: "stereo", AllowUpmix: true},
		Colors: map[string]*ColorDecl{
			"v1": {Primaries: 1, Transfer: 1, Matrix: 1},
			"v2": {Primaries: 1, Transfer: 1, Matrix: 1},
		},
		KeyEvery: 30,
	}
}

func writeProject(root string, proj *Project) (string, error) {
	path := filepath.Join(root, "project.json")
	data, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o600)
}

func checksum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
