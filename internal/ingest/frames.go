// Package ingest parses frame records from JSONL input files using streaming
// reads, verifies every SHA-256 checksum against the embedded RTP payload and
// produces the input digest later used by the checkpoint store to detect
// changed inputs. Defects are reported as model.Fault records that carry the
// track, frame sequence number and exact byte offset of the bad record.
package ingest

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/mediamend/mediamend/internal/model"
	"github.com/pion/rtp"
)

// Frame is one parsed input record. RTP is only set when the record embeds a
// packet; SourceSHA is the checksum recorded by the capturing device and is
// never rewritten by any downstream stage.
type Frame struct {
	TrackID    string         `json:"track_id"`
	Seq        uint64         `json:"frame_seq"`
	DeviceTS   uint64         `json:"device_ts"`
	Rate       uint64         `json:"rate"`
	Payload    []byte         `json:"-"` // RTP packet bytes (decoded)
	SourceSHA  string         `json:"sha256"`
	Color      *ColorMeta     `json:"color,omitempty"`
	Raw        map[string]any `json:"-"`
	Offset     int64          `json:"-"` // byte offset of the record line
	LineNo     int            `json:"-"`
	ChecksumOK bool           `json:"-"`
}

// ColorMeta is the color description carried by an individual frame, if any.
type ColorMeta struct {
	Primaries uint16 `json:"primaries"`
	Transfer  uint16 `json:"transfer"`
	Matrix    uint16 `json:"matrix"`
	// FullRange distinguishes full-range (1) from limited-range (0) samples.
	FullRange bool `json:"full_range"`
}

// rawRecord is the on-disk JSONL wire format.
type rawRecord struct {
	TrackID  string          `json:"track_id"`
	Seq      uint64          `json:"frame_seq"`
	DeviceTS uint64          `json:"device_ts"`
	Rate     uint64          `json:"rate"`
	RTPB64   string          `json:"rtp_b64"`
	SHA      string          `json:"sha256"`
	Color    *ColorMeta      `json:"color,omitempty"`
}

// Reader streams frames from one JSONL file. It is safe only for single
// goroutine use; the pipeline opens one Reader per track instead.
type Reader struct {
	file    string
	trackID string
	sc      *bufio.Scanner
	f       *os.File
	offset  int64
	lineNo  int
	digest  [32]byte // running hash over the raw bytes of the file
	faults  []model.Fault
	closed  bool
}

const maxRecordBytes = 4 * 1024 * 1024 // 4 MiB per JSONL line

// Open opens a JSONL frame file for the given logical track.
func Open(file, trackID string) (*Reader, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxRecordBytes)
	return &Reader{file: file, trackID: trackID, sc: sc, f: f}, nil
}

// Next returns the next frame, or io.EOF when the stream is exhausted. A
// checksum failure is returned both as a fault inside the Reader and as a
// frame carrying ChecksumOK=false, so callers can keep their stream logic in
// one place.
func (r *Reader) Next() (*Frame, error) {
	for r.sc.Scan() {
		line := r.sc.Bytes()
		start := r.offset
		r.offset += int64(len(line)) + 1 // scanner strips the trailing '\n'
		r.lineNo++
		r.digest = sha256.Sum256(append(r.digest[:], line...))
		if len(line) == 0 {
			continue
		}
		var rr rawRecord
		if err := json.Unmarshal(line, &rr); err != nil {
			r.faults = append(r.faults, model.Fault{
				Class:    model.FaultChecksum,
				Severity: model.SeverityFatal,
				Location: model.SourceLocation{File: r.file, Track: r.trackID, Seq: 0, Offset: start},
				Message:  fmt.Sprintf("malformed JSONL record on line %d: %v", r.lineNo, err),
			})
			continue
		}
		fr := &Frame{
			TrackID: r.trackID, Seq: rr.Seq, DeviceTS: rr.DeviceTS, Rate: rr.Rate,
			SourceSHA: rr.SHA, Color: rr.Color, Offset: start, LineNo: r.lineNo,
		}
		if rr.TrackID != "" {
			fr.TrackID = rr.TrackID
		}
		payload, derr := base64.StdEncoding.DecodeString(rr.RTPB64)
		if derr != nil {
			r.faults = append(r.faults, model.Fault{
				Class:    model.FaultChecksum,
				Severity: model.SeverityFatal,
				Location: r.loc(fr),
				Message:  fmt.Sprintf("invalid base64 payload: %v", derr),
			})
			continue
		}
		fr.Payload = payload
		sum := sha256.Sum256(payload)
		got := hex.EncodeToString(sum[:])
		fr.ChecksumOK = got == rr.SHA
		if !fr.ChecksumOK {
			r.faults = append(r.faults, model.Fault{
				Class:       model.FaultChecksum,
				Severity:    model.SeverityFatal,
				Location:    r.loc(fr),
				Message:     "frame payload does not match recorded SHA-256",
				Expected:    rr.SHA,
				Actual:      got,
				SourceCheck: rr.SHA,
			})
		} else if err := (&rtp.Packet{}).Unmarshal(payload); err != nil {
			r.faults = append(r.faults, model.Fault{
				Class:       model.FaultChecksum,
				Severity:    model.SeverityFatal,
				Location:    r.loc(fr),
				Message:     fmt.Sprintf("payload is not a valid RTP packet: %v", err),
				SourceCheck: rr.SHA,
			})
			fr.ChecksumOK = false
		}
		return fr, nil
	}
	if err := r.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (r *Reader) loc(fr *Frame) model.SourceLocation {
	return model.SourceLocation{File: r.file, Track: fr.TrackID, Seq: fr.Seq, Offset: fr.Offset}
}

// Faults returns faults accumulated so far (checksum and parse failures).
func (r *Reader) Faults() []model.Fault { return r.faults }

// InputDigest is the SHA-256 over every raw record byte read so far.
func (r *Reader) InputDigest() string { return hex.EncodeToString(r.digest[:]) }

// Close releases the underlying file.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.f.Close()
}

// ReadAll drains a Reader into memory, returning frames in stream order plus
// the input digest and all faults. The 10k-frame demo stays well under the
// memory budget; the streaming Reader remains available for larger inputs.
func ReadAll(r *Reader) ([]*Frame, string, []model.Fault, error) {
	var frames []*Frame
	for {
		fr, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return frames, r.InputDigest(), r.Faults(), err
		}
		frames = append(frames, fr)
	}
	return frames, r.InputDigest(), r.Faults(), nil
}

// TrackInput names one JSONL file and the logical track it belongs to.
type TrackInput struct {
	TrackID string
	File    string
}

// DigestInputs computes a deterministic combined SHA-256 over all input
// files in sorted track order. This is the checkpoint "input summary": any
// change to any frame file changes the digest and forces a fresh run.
func DigestInputs(inputs []TrackInput) (string, error) {
	sorted := append([]TrackInput(nil), inputs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TrackID < sorted[j].TrackID })
	h := sha256.New()
	for _, in := range sorted {
		raw, err := os.ReadFile(in.File)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s:%d:", in.TrackID, len(raw))
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
