package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"tsqc/internal/ts"
)

const (
	// PMTPID is the regenerated program map PID.
	PMTPID uint16 = 0x1000
	// ProgramNumber is the single program in every output stream.
	ProgramNumber uint16 = 1
)

// Segment is one independently playable chunk beginning at an IDR.
type Segment struct {
	Index      int
	StartPTS   int64
	EndPTS     int64
	Keyframe   bool
	VideoAUs   int
	AudioAUs   int
	GapBack    bool // the segment begins after an unrecoverable interval
	Bytes      []byte
	SHA256     string
	ContentSHA string
	InputSHA   string
	Size       int
}

// Digest returns the hex SHA-256 of the segment bytes.
func (s *Segment) Digest() string {
	if s.SHA256 != "" {
		return s.SHA256
	}
	sum := sha256.Sum256(s.Bytes)
	s.SHA256 = hex.EncodeToString(sum[:])
	return s.SHA256
}

// Segmenter cuts access units at IDR boundaries and re-muxes each segment
// with fresh PAT/PMT and continuous PCR.
type Segmenter struct {
	VideoPID uint16
	AudioPID uint16
	PCRPID   uint16
	// PCRInterval is how many video access units may pass between PCRs.
	PCRInterval int
}

// DefaultSegmenter returns the canonical mux configuration.
func DefaultSegmenter(videoPID, audioPID uint16) *Segmenter {
	return &Segmenter{VideoPID: videoPID, AudioPID: audioPID, PCRPID: videoPID, PCRInterval: 2}
}

type segItem struct {
	au   ts.AccessUnit
	pts  int64
	kind ts.Kind
}

func (s segItem) pid() uint16 { return s.au.PID }

func auLess(a, b *ts.AccessUnit) bool {
	if a.PTS >= 0 && b.PTS >= 0 && a.PTS != b.PTS {
		return a.PTS < b.PTS
	}
	if a.PTS >= 0 && b.PTS < 0 {
		return true
	}
	if a.PTS < 0 && b.PTS >= 0 {
		return false
	}
	if a.PID != b.PID {
		return a.PID < b.PID
	}
	return a.Kind < b.Kind
}

func itemLess(a, b segItem) bool { return auLess(&a.au, &b.au) }

// Summary renders a digest line for terminal output.
func (s *Segment) Summary() string {
	return fmt.Sprintf("segment=%02d pts=%d..%d video=%d audio=%d sha256=%s", s.Index, s.StartPTS, s.EndPTS, s.VideoAUs, s.AudioAUs, s.Digest())
}

// Build splits the access unit stream into segments. Leading units before
// the first IDR are dropped; an unrecoverable unit closes the current
// segment so the next IDR starts a clean one.
