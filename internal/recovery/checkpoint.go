// Package recovery persists trusted processing boundaries and resumes
// interrupted runs idempotently from the last committed checkpoint.
package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"tscheck/internal/ts"
)

// Checkpoint records a trusted packet boundary. Checkpoints are only
// ever committed at complete, trusted packet boundaries.
type Checkpoint struct {
	Branch      string `json:"branch"`
	PacketIndex int    `json:"packet_index"` // trusted packets committed
	ByteOffset  int64  `json:"byte_offset"`  // source offset after that packet
	OutputBytes int64  `json:"output_bytes"` // trusted output bytes committed
	SHA256      string `json:"sha256"`       // hash of the committed output
}

// Store persists one checkpoint file per branch under Dir.
type Store struct{ Dir string }

func (s Store) path(branch string) string {
	return filepath.Join(s.Dir, "checkpoint-"+branch+".json")
}

// Save writes a checkpoint atomically (temp file + rename).
func (s Store) Save(cp Checkpoint) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp := s.path(cp.Branch) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(cp.Branch))
}

// Load reads the checkpoint for branch, if any.
func (s Store) Load(branch string) (Checkpoint, bool, error) {
	b, err := os.ReadFile(s.path(branch))
	if os.IsNotExist(err) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return Checkpoint{}, false, fmt.Errorf("recovery: bad checkpoint: %w", err)
	}
	return cp, true, nil
}

// TrustedPackets scans data and returns the packets passing sync,
// continuity and PSI checks — the only packets a checkpoint may commit.
func TrustedPackets(data []byte) ([]ts.PacketInfo, *ts.Report) {
	pkts, rep := ts.NewScanner().Scan(data)
	bad := map[int64]bool{}
	for _, f := range rep.Faults {
		bad[f.Offset] = true
	}
	psi := ts.NewPSITracker()
	var trusted []ts.PacketInfo
	for _, pi := range pkts {
		for _, f := range psi.Feed(pi) {
			bad[f.Offset] = true
		}
		if !bad[pi.Offset] {
			trusted = append(trusted, pi)
		}
	}
	return trusted, rep
}

// Result summarizes one Process call.
type Result struct {
	SHA256      string
	OutputBytes int64
	ResumedFrom int  // trusted packets skipped via checkpoint
	Done        bool // false when interrupted before completion
}

// Runner processes inputs with checkpoints.
type Runner struct {
	Store Store
	Every int // checkpoint interval in trusted packets (default 1024)
}

// Process writes the trusted output TS to outPath. A prior checkpoint
// resumes the run; interruptAt > 0 simulates a crash after that many
// trusted packets (no further checkpoint is committed). Re-running after
// completion is a no-op that yields the identical hash.
func (r Runner) Process(branch string, data []byte, outPath string, interruptAt int) (Result, error) {
	trusted, _ := TrustedPackets(data)
	every := r.Every
	if every <= 0 {
		every = 1024
	}
	start := 0
	h := sha256.New()
	var written int64
	if cp, ok, err := r.Store.Load(branch); err != nil {
		return Result{}, err
	} else if ok && cp.PacketIndex > 0 {
		if fi, err := os.Stat(outPath); err == nil && fi.Size() >= cp.OutputBytes {
			if err := os.Truncate(outPath, cp.OutputBytes); err != nil {
				return Result{}, err
			}
			b, err := os.ReadFile(outPath)
			if err != nil {
				return Result{}, err
			}
			h.Write(b)
			start = cp.PacketIndex
			written = cp.OutputBytes
		}
	}
	if start > len(trusted) {
		start = len(trusted)
	}
	mode := os.O_CREATE | os.O_WRONLY
	if start == 0 {
		mode |= os.O_TRUNC
	} else {
		mode |= os.O_APPEND
	}
	f, err := os.OpenFile(outPath, mode, 0o644)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	res := Result{ResumedFrom: start}
	for i := start; i < len(trusted); i++ {
		off := trusted[i].Offset
		pkt := data[off : off+ts.PacketSize]
		if _, err := f.Write(pkt); err != nil {
			return res, err
		}
		h.Write(pkt)
		written += ts.PacketSize
		n := i + 1
		if interruptAt > 0 && n >= interruptAt && n < len(trusted) {
			res.SHA256 = hex.EncodeToString(h.Sum(nil))
			res.OutputBytes = written
			return res, nil // crash: no checkpoint for uncommitted packets
		}
		if n%every == 0 || n == len(trusted) {
			cp := Checkpoint{
				Branch:      branch,
				PacketIndex: n,
				ByteOffset:  off + ts.PacketSize,
				OutputBytes: written,
				SHA256:      hex.EncodeToString(h.Sum(nil)),
			}
			if err := r.Store.Save(cp); err != nil {
				return res, err
			}
		}
	}
	res.SHA256 = hex.EncodeToString(h.Sum(nil))
	res.OutputBytes = written
	res.Done = true
	return res, nil
}
