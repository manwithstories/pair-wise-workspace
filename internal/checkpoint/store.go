// Package checkpoint atomically saves and restores recovery state with
// CRC validation and fallback to the most recent valid generation.
package checkpoint

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"

	"mediad/internal/ingest"
	"mediad/internal/media"
	"mediad/internal/normalize"
	"mediad/internal/recovery"
	"mediad/internal/timeline"
)

var magic = []byte("MCK1")

// TrackState is the checkpointed state of one track.
type TrackState struct {
	SSRC uint32
	// signaling parameters
	ClockRate int64
	Channels  int
	Layout    string
	IsVideo   bool
	// ingest reconstruction state
	Started bool
	MaxSeq  uint16
	SeqROC  uint64
	MaxTS   uint32
	TSROC   int64
	// clock mapping
	Anchored  bool
	RTPAnchor int64
	NTPAnchor int64
	Jumps     int
	// recovery
	Expected uint64
	Missing  []uint64
	Stats    recovery.Stats
	// resampler position
	ResamplerPos int64
	// newest pushed event time of this track (emission horizon input)
	LastPush int64
}

// Counters are the pipeline counters that must survive a restart.
type Counters struct {
	Duplicates    int64
	Reordered     int64
	Loss          int64 // currently open, fillable
	Unrecoverable int64
	LayoutErrors  int64
	Rewinds       int64
	LateDrops     int64
	FirstFault    int64 // input arrival index of the first fault, -1 when none
}

// QueuedPacket is one packet held in a jitter queue at checkpoint time.
type QueuedPacket struct {
	Track  uint32
	Packet *ingest.Packet
	Time   media.MediaTime
}

// State is a full recovery checkpoint.
type State struct {
	Generation   uint64
	Arrival      int64
	Watermark    int64
	WatermarkSet bool
	MaxPushed    int64
	OutputOffset int64
	Tracks       []TrackState
	Queued       []QueuedPacket
	Retained     []*timeline.Event
	Colors       map[uint32]normalize.ColorInfo
	Diagnostics  []string
	Counters     Counters
}

// Store keeps the two most recent checkpoint generations on disk.
type Store struct {
	path string
}

func New(path string) *Store { return &Store{path: path} }

func (s *Store) slotPath(gen uint64) string {
	return fmt.Sprintf("%s.%d", s.path, gen%2)
}

// CurrentPath returns the slot the newest valid generation occupies, or
// the would-be path of generation 0 when nothing is stored yet.
func (s *Store) CurrentPath() string {
	st, _, err := s.Load()
	if err != nil || st == nil {
		return s.slotPath(0)
	}
	return s.slotPath(st.Generation)
}

// Save atomically writes st into its generation slot.
func (s *Store) Save(st *State) error {
	payload, err := json.Marshal(st)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(payload)+8)
	buf = append(buf, magic...)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(payload))
	buf = append(buf, crc[:]...)
	buf = append(buf, payload...)
	final := s.slotPath(st.Generation)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func parse(raw []byte) (*State, error) {
	if len(raw) < 8 || string(raw[:4]) != string(magic) {
		return nil, fmt.Errorf("bad magic or truncated header")
	}
	want := binary.BigEndian.Uint32(raw[4:8])
	payload := raw[8:]
	if crc32.ChecksumIEEE(payload) != want {
		return nil, fmt.Errorf("checksum mismatch")
	}
	var st State
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Load returns the newest valid checkpoint. A corrupt or truncated
// generation is reported as a checkpoint-corrupt fault and the loader
// falls back to the previous valid generation; only when both are
// unusable is an error returned.
func (s *Store) Load() (*State, *media.Fault, error) {
	var states []*State
	var fallback *media.Fault
	for slot := 0; slot < 2; slot++ {
		raw, err := os.ReadFile(s.slotPath(uint64(slot)))
		if err != nil {
			continue
		}
		st, err := parse(raw)
		if err != nil {
			fallback = &media.Fault{
				Kind: media.FaultCheckpointCorrupt, Op: "checkpoint.load",
				Msg: fmt.Sprintf("slot %d invalid (%v), falling back", slot, err),
			}
			continue
		}
		states = append(states, st)
	}
	if len(states) == 0 {
		if fallback != nil {
			return nil, fallback, fmt.Errorf("no valid checkpoint: %s", fallback.Msg)
		}
		return nil, nil, os.ErrNotExist
	}
	best := states[0]
	for _, st := range states[1:] {
		if st.Generation > best.Generation {
			best = st
		}
	}
	return best, fallback, nil
}

// Remove deletes both generations, e.g. for a fresh run.
func (s *Store) Remove() {
	for slot := 0; slot < 2; slot++ {
		os.Remove(s.slotPath(uint64(slot)))
	}
}
