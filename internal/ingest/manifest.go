package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"

	"pairflow/internal/model"
)

// AlgorithmVersion is re-exported for manifest consumers.
const AlgorithmVersion = model.AlgorithmVersion

// Source identifies the physical or generated origin of a manifest.
type Source struct {
	Fingerprint string `json:"fingerprint"`
	Kind        string `json:"kind"`
	Locator     string `json:"locator,omitempty"`
}

// FrameData couples frame provenance with its immutable payload bytes.
type FrameData struct {
	Meta    model.Frame `json:"meta"`
	Payload []byte      `json:"-"`
}

// Manifest is the validated boundary between ingest and the pipeline.
type Manifest struct {
	AlgorithmVersion string            `json:"algorithm_version"`
	Source           Source            `json:"source"`
	Tracks           []model.Track     `json:"tracks"`
	Frames           []FrameData       `json:"-"`
	Audio            *model.AudioTrack `json:"audio,omitempty"`
}

// Sum256 returns the lowercase hex SHA-256 of data.
func Sum256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NewLocator builds log2-sized XOR parity equations over byte offsets. For a
// payload of length n, one parity byte is produced for every bit needed to
// represent an offset. A single changed byte can then be located exactly.
func NewLocator(payload []byte) ([]byte, uint64) {
	bits := 1
	for n := len(payload); n > 1; n >>= 1 {
		bits++
	}
	parity := make([]byte, bits)
	for off, b := range payload {
		for bit := 0; bit < bits; bit++ {
			if off&(1<<bit) != 0 {
				parity[bit] ^= b
			}
		}
	}
	return parity, uint64(len(payload))
}

// LocateSingleByteDamage returns the payload-relative offset of one changed
// byte. It returns -1 when there is no mismatch or the parity pattern is not a
// valid single-byte syndrome.
func LocateSingleByteDamage(payload, expectedParity []byte, payloadLen uint64) int {
	if payloadLen == 0 || int(payloadLen) != len(payload) || len(expectedParity) == 0 {
		return -1
	}
	actual, _ := NewLocator(payload)
	if len(actual) != len(expectedParity) {
		return -1
	}
	var delta byte
	offset := 0
	syndrome := make([]byte, len(actual))
	for bit := range actual {
		syndrome[bit] = actual[bit] ^ expectedParity[bit]
		if syndrome[bit] != 0 {
			if delta == 0 {
				delta = syndrome[bit]
			} else if syndrome[bit] != delta {
				return -1
			}
			offset |= 1 << bit
		}
	}
	if delta == 0 {
		return -1
	}
	if offset < 0 || offset >= len(payload) {
		return -1
	}
	// Verify the candidate explains every parity equation.
	repaired := append([]byte(nil), payload...)
	repaired[offset] ^= delta
	repairedParity, _ := NewLocator(repaired)
	for bit := range repairedParity {
		if repairedParity[bit] != expectedParity[bit] {
			return -1
		}
	}
	return offset
}

// ValidateMetadata checks identity, ordering and required provenance fields.
func (m *Manifest) ValidateMetadata() error {
	if m == nil {
		return &model.MediaError{Code: model.CodeMetadataInvalid, Message: "nil manifest"}
	}
	if m.AlgorithmVersion == "" {
		return &model.MediaError{Code: model.CodeMetadataInvalid, Message: "missing algorithm version"}
	}
	if m.Source.Fingerprint == "" {
		return &model.MediaError{Code: model.CodeUnknownSource, Message: "missing source fingerprint"}
	}
	seenTracks := make(map[string]bool, len(m.Tracks))
	for i := range m.Tracks {
		t := &m.Tracks[i]
		if t.ID == "" {
			return &model.MediaError{Code: model.CodeMetadataInvalid, Message: fmt.Sprintf("track %d has no id", i)}
		}
		if seenTracks[t.ID] {
			return &model.MediaError{Code: model.CodeMetadataInvalid, TrackID: t.ID, Message: "duplicate track id"}
		}
		seenTracks[t.ID] = true
		if t.ClockRateHz == 0 {
			return &model.MediaError{Code: model.CodeMetadataInvalid, TrackID: t.ID, Message: "missing clock rate"}
		}
	}
	lastSeq := map[string]int64{}
	for i := range m.Frames {
		f := &m.Frames[i].Meta
		if !seenTracks[f.TrackID] {
			return &model.MediaError{Code: model.CodeUnknownSource, TrackID: f.TrackID, Message: "frame references unknown track"}
		}
		if f.SHA256 == "" {
			return &model.MediaError{Code: model.CodeMetadataInvalid, TrackID: f.TrackID, Message: "frame missing sha256"}
		}
		if prev, ok := lastSeq[f.TrackID]; ok && f.Sequence <= prev {
			return &model.MediaError{Code: model.CodeMetadataInvalid, TrackID: f.TrackID, Message: "frame sequences must increase"}
		}
		lastSeq[f.TrackID] = f.Sequence
	}
	return nil
}

// VerifyFrame checks one frame and returns the unified mismatch record.
func VerifyFrame(f *model.Frame, payload []byte) *model.MediaError {
	if f == nil {
		return &model.MediaError{Code: model.CodeMetadataInvalid, Message: "nil frame metadata", Recoverable: false}
	}
	actual := Sum256(payload)
	if actual == f.SHA256 {
		return nil
	}
	return &model.MediaError{
		Code:              model.CodeDigestMismatch,
		TrackID:           f.TrackID,
		ExtendedTimestamp: f.ExtendedTimestamp,
		SourceByteOffset:  f.SourceOffset,
		ExpectedDigest:    f.SHA256,
		ActualDigest:      actual,
		Impact: model.Interval{
			StartNS:  f.PTSNS,
			EndNS:    f.PTSNS + f.DurationNS,
			StartSeq: f.Sequence,
			EndSeq:   f.Sequence,
		},
		Message:     "frame SHA-256 mismatch",
		Recoverable: false,
	}
}

// HashState exposes hash.Hash for streaming source verification.
func HashState() hash.Hash { return sha256.New() }
