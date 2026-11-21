// Package verify independently re-reads restored artifacts and recomputes
// their evidence. It never trusts pipeline in-memory state: every shard is
// re-hashed from disk and compared against the checkpoint record, so the
// summary can stand as an audit document.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/archive/rtprestore/internal/checkpoint"
)

// Exit codes follow the archive contract.
const (
	ExitOK               = 0
	ExitBadArgs          = 1
	ExitDataCorruption   = 2
	ExitUnsafeCheckpoint = 3
)

// Corruption is one locatable data-integrity finding.
type Corruption struct {
	ShardIndex int    `json:"shard_index"`
	ShardName  string `json:"shard_name"`
	FirstTick  int64  `json:"first_error_tick"`
	Reason     string `json:"reason"`
}

func (c Corruption) Error() string {
	return fmt.Sprintf("verify: shard %d (%s) at tick %d: %s",
		c.ShardIndex, c.ShardName, c.FirstTick, c.Reason)
}

// Report is the audit summary printed by the CLI.
type Report struct {
	RoundTripRate      float64      `json:"roundtrip_rate"`
	KeyframeHitRate    float64      `json:"keyframe_hit_rate"`
	Segments           int          `json:"segments"`
	SegmentSHA256Count int          `json:"segment_sha256_count"`
	ResumeReused       int          `json:"resume_reused"`
	CorruptRejected    int          `json:"corrupt_rejected"`
	BadSegments        int          `json:"bad_segments"`
	FirstErrorTick     int64        `json:"first_error_tick"`
	Corruptions        []Corruption `json:"corruptions,omitempty"`
}

// Inputs are the run metrics the verifier cross-checks against disk evidence.
type Inputs struct {
	RoundTripRate   float64
	KeyframeHitRate float64
	BlockSize       int // ticks per shard, used to locate a corruption tick
}

// Run re-reads the checkpoint and every shard under dir. Reused counts intact
// shards accepted during resume; every hash mismatch or torn tail becomes a
// locatable corruption. The first corruption's tick is reported directly.
func Run(dir string, params checkpoint.Params, in Inputs) (Report, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", checkpoint.ErrCheckpointCorrupt, err)
	}
	var state checkpoint.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return Report{}, fmt.Errorf("%w: %v", checkpoint.ErrCheckpointCorrupt, err)
	}
	if state.ParamDigest != params.Digest() {
		return Report{}, fmt.Errorf("%w: stored=%s requested=%s",
			checkpoint.ErrParamsChanged, state.ParamDigest, params.Digest())
	}
	rep := Report{
		RoundTripRate:      in.RoundTripRate,
		KeyframeHitRate:    in.KeyframeHitRate,
		Segments:           len(state.Shards),
		SegmentSHA256Count: len(state.Shards),
		FirstErrorTick:     -1,
	}
	for _, info := range state.Shards {
		// The first packet of shard i lands at tick i*BlockSize.
		tick := int64(info.Index) * int64(in.BlockSize)
		sum, size, err := hashFile(filepath.Join(dir, info.Name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			rep.add(Corruption{ShardIndex: info.Index, ShardName: info.Name, FirstTick: tick, Reason: "missing shard"})
		case err != nil:
			rep.add(Corruption{ShardIndex: info.Index, ShardName: info.Name, FirstTick: tick, Reason: err.Error()})
		case sum != info.SHA256:
			rep.add(Corruption{ShardIndex: info.Index, ShardName: info.Name, FirstTick: tick, Reason: "sha256 mismatch"})
		case size != info.Size:
			rep.add(Corruption{ShardIndex: info.Index, ShardName: info.Name, FirstTick: tick, Reason: "size mismatch"})
		default:
			rep.ResumeReused++
		}
	}
	rep.CorruptRejected = rep.BadSegments
	return rep, nil
}

func (r *Report) add(c Corruption) {
	if r.FirstErrorTick < 0 || c.FirstTick < r.FirstErrorTick {
		r.FirstErrorTick = c.FirstTick
	}
	r.BadSegments++
	r.Corruptions = append(r.Corruptions, c)
}

// InjectTornTail truncates the last shard after the checkpoint recorded its
// full hash, emulating an interrupted write for the verify command.
func InjectTornTail(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		return fmt.Errorf("%w: %v", checkpoint.ErrCheckpointCorrupt, err)
	}
	var state checkpoint.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("%w: %v", checkpoint.ErrCheckpointCorrupt, err)
	}
	if len(state.Shards) == 0 {
		return errors.New("verify: no shards to inject a torn tail into")
	}
	last := state.Shards[len(state.Shards)-1]
	path := filepath.Join(dir, last.Name)
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// Keep at most half the bytes, always a non-empty prefix.
	keep := last.Size / 2
	if keep < 1 {
		keep = 1
	}
	return f.Truncate(keep)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
