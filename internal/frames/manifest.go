// Package frames parses and validates frame-sequence manifests.  A manifest
// describes one track's frames: sequence number, media timestamp, keyframe
// flag, payload size, SHA-256 checksum and the RTP packet header the frame
// was carried in.  Validation is defined to report the earliest bad frame,
// never the last one or an arbitrary one, because operators resume from that
// frame.
package frames

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
)

// Limits bounds the size of untrusted input.
type Limits struct {
	MaxFrameBytes int
	MaxFrames     int
}

// DefaultLimits are deliberately small: mediapack is a local tool, not a
// transcoder, and an oversized manifest must be rejected rather than loaded.
func DefaultLimits() Limits {
	return Limits{MaxFrameBytes: 1 << 20, MaxFrames: 10000}
}

// Record is the on-disk description of one frame.
type Record struct {
	Number    int    `json:"number"`
	Timestamp uint64 `json:"timestamp"`
	Keyframe  bool   `json:"keyframe"`
	SHA256    string `json:"sha256"`
	RTPPacket []byte `json:"rtp_packet"` // base64 in JSON
}

// Manifest is one track's frame sequence.
type Manifest struct {
	Track     string   `json:"track"`
	Timescale uint32   `json:"timescale"`
	Records   []Record `json:"frames"`
}

// Frame is a validated frame with its exact tick timestamp.
type Frame struct {
	Track     string
	Number    int
	Timestamp uint64
	Timescale uint32
	Ticks     clock.Ticks
	Keyframe  bool
	Size      int
	SHA256    string
	RTPSeq    uint16
	RTPTS     uint32
}

// Sum returns the lowercase hex SHA-256 of payload.
func Sum(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ValidSHA256 reports whether s is a lowercase 64-character hex digest.
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == s
}

// Parse decodes a manifest and applies structural limits.  Unknown JSON
// fields are rejected so typos cannot silently drop a checksum.
func Parse(data []byte, limits Limits) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fault.Wrap(fault.ClassInput, "frames", "*", "invalid-manifest",
			"manifest is not valid JSON", err)
	}
	if m.Track == "" {
		return nil, fault.New(fault.ClassInput, "frames", "*", "invalid-manifest", "empty track name")
	}
	if m.Timescale == 0 {
		return nil, fault.New(fault.ClassInput, "frames", m.Track, "invalid-manifest", "zero timescale")
	}
	if len(m.Records) > limits.MaxFrames {
		return nil, fault.New(fault.ClassLimit, "frames", m.Track, "too-many-frames",
			fmt.Sprintf("%d frames exceeds limit %d", len(m.Records), limits.MaxFrames))
	}
	return &m, nil
}

// Validate checks a manifest and returns frames in sequence order.  On
// failure it returns nil and exactly one fault that names the earliest bad
// frame, chosen by frame number.
func Validate(m *Manifest, payloads map[int][]byte, limits Limits) ([]Frame, error) {
	if m == nil {
		return nil, fault.New(fault.ClassInternal, "frames", "*", "nil-manifest", "nil manifest")
	}
	if len(m.Records) > limits.MaxFrames {
		return nil, fault.New(fault.ClassLimit, "frames", m.Track, "too-many-frames",
			fmt.Sprintf("%d frames exceeds limit %d", len(m.Records), limits.MaxFrames))
	}

	var errs []error
	seen := make(map[int]bool, len(m.Records))
	byNumber := make(map[int]Record, len(m.Records))
	minNum, maxNum := 0, 0

	for _, rec := range m.Records {
		if rec.Number <= 0 {
			errs = append(errs, frameErr(m.Track, rec.Number, "invalid-sequence", "frame number must be positive"))
			continue
		}
		if seen[rec.Number] {
			errs = append(errs, frameErr(m.Track, rec.Number, "duplicate-sequence", "frame number appears more than once"))
			continue
		}
		seen[rec.Number] = true
		byNumber[rec.Number] = rec
		if minNum == 0 || rec.Number < minNum {
			minNum = rec.Number
		}
		if rec.Number > maxNum {
			maxNum = rec.Number
		}
	}

	// Missing frames are checked from the first number to the last; a gap's
	// earliest absent number is the frame operators must recover.
	for n := minNum; n > 0 && n <= maxNum; n++ {
		if !seen[n] {
			errs = append(errs, frameErr(m.Track, n, "missing-frame", "frame number is absent from the sequence"))
		}
	}

	numbers := make([]int, 0, len(byNumber))
	for n := range byNumber {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)

	out := make([]Frame, 0, len(numbers))
	prevTS := int64(-1)
	prevSeq := -1
	prevRTPTS := int64(-1)

	for _, n := range numbers {
		rec := byNumber[n]
		payload, ok := payloads[n]
		if !ok {
			errs = append(errs, frameErr(m.Track, n, "missing-payload", "frame payload is unavailable"))
			continue
		}
		if len(payload) > limits.MaxFrameBytes {
			errs = append(errs, fault.New(fault.ClassLimit, "frames", m.Track, "frame-too-large",
				fmt.Sprintf("frame %d: %d bytes exceeds limit %d", n, len(payload), limits.MaxFrameBytes)).WithFrame(n))
			continue
		}
		if !ValidSHA256(rec.SHA256) {
			errs = append(errs, frameErr(m.Track, n, "invalid-checksum", "checksum is not 64 lowercase hex characters"))
			continue
		}
		if got := Sum(payload); got != rec.SHA256 {
			errs = append(errs, frameErr(m.Track, n, "checksum-mismatch",
				"payload digest does not match manifest"))
			continue
		}
		if int64(rec.Timestamp) <= prevTS {
			errs = append(errs, frameErr(m.Track, n, "non-monotonic-time", "timestamp does not advance"))
			continue
		}

		// RTP timestamps are only accepted when the manifest timestamp is
		// exactly representable on the 90 kHz RTP clock.  Rounding here would
		// make segment boundaries differ between reruns.
		wantRTP, convErr := clock.ConvertExact(int64(rec.Timestamp), m.Timescale, 90000)
		if convErr != nil || wantRTP < 0 || wantRTP > int64(^uint32(0)) {
			errs = append(errs, frameErr(m.Track, n, "unrepresentable-time",
				fmt.Sprintf("timestamp %d/%d cannot be expressed exactly on 90 kHz", rec.Timestamp, m.Timescale)))
			continue
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(rec.RTPPacket); err != nil {
			errs = append(errs, fault.Wrap(fault.ClassInput, "frames", m.Track,
				"invalid-rtp", "RTP packet header is malformed", err).WithFrame(n))
			continue
		}
		if pkt.Version != 2 {
			errs = append(errs, frameErr(m.Track, n, "invalid-rtp", "RTP version is not 2"))
			continue
		}
		if int64(pkt.Timestamp) != wantRTP {
			errs = append(errs, frameErr(m.Track, n, "rtp-time-mismatch",
				fmt.Sprintf("RTP timestamp %d does not match manifest %d", pkt.Timestamp, wantRTP)))
			continue
		}
		if prevSeq >= 0 && int(pkt.SequenceNumber) != prevSeq+1 {
			errs = append(errs, frameErr(m.Track, n, "rtp-sequence-gap",
				fmt.Sprintf("RTP sequence %d does not follow %d", pkt.SequenceNumber, prevSeq)))
			continue
		}
		if prevRTPTS >= 0 && int64(pkt.Timestamp) <= prevRTPTS {
			errs = append(errs, frameErr(m.Track, n, "non-monotonic-time", "RTP timestamp does not advance"))
			continue
		}

		ticks, err := clock.FromTimestamp(rec.Timestamp, m.Timescale)
		if err != nil {
			errs = append(errs, frameErr(m.Track, n, "unrepresentable-time", err.Error()))
			continue
		}

		prevTS = int64(rec.Timestamp)
		prevSeq = int(pkt.SequenceNumber)
		prevRTPTS = int64(pkt.Timestamp)
		out = append(out, Frame{
			Track:     m.Track,
			Number:    n,
			Timestamp: rec.Timestamp,
			Timescale: m.Timescale,
			Ticks:     ticks,
			Keyframe:  rec.Keyframe,
			Size:      len(payload),
			SHA256:    rec.SHA256,
			RTPSeq:    pkt.SequenceNumber,
			RTPTS:     pkt.Timestamp,
		})
	}

	if len(errs) > 0 {
		return nil, earliest(errs)
	}
	return out, nil
}

func frameErr(track string, n int, code, msg string) *fault.Error {
	return fault.New(fault.ClassInput, "frames", track, code, msg).WithFrame(n)
}

// earliest returns the fault with the smallest frame number, then stage.
func earliest(errs []error) error {
	fe, ok := fault.As(errs[0])
	if !ok {
		return errs[0]
	}
	for _, err := range errs[1:] {
		next, ok := fault.As(err)
		if !ok {
			continue
		}
		if next.Frame < fe.Frame || (next.Frame == fe.Frame && next.Code < fe.Code) {
			fe = next
		}
	}
	return fe
}

// RTPTimestamp derives the RTP timestamp for a frame, failing when the
// conversion from its native timescale is not exact.
func RTPTimestamp(ts uint64, timescale uint32) (uint32, error) {
	v, err := clock.ConvertExact(int64(ts), timescale, 90000)
	if err != nil {
		return 0, err
	}
	if v < 0 || v > int64(^uint32(0)) {
		return 0, fmt.Errorf("RTP timestamp out of range: %d", v)
	}
	return uint32(v), nil
}
