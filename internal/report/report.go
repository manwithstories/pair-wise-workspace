// Package report builds audit results and writes their terminal/JSON forms.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	auditrtp "rtspaudit/internal/rtp"
)

// PullReport is the stable JSON schema for one transport run.
type PullReport struct {
	Transport          string         `json:"transport"`
	StateTransitions   int            `json:"state_transitions"`
	IllegalTransitions int            `json:"illegal_transitions"`
	DuplicatePackets   uint64         `json:"duplicate_packets"`
	LateDropped        uint64         `json:"late_dropped"`
	RTPSeqGaps         []auditrtp.Gap `json:"rtp_seq_gaps"`
	FramesReassembled  int            `json:"frames_reassembled"`
	DroppedFrames      int            `json:"dropped_frames"`
	Keyframes          int            `json:"keyframes"`
	Bytes              int            `json:"bytes"`
	FrameHashes        []string       `json:"frame_hashes"`
}

// CompareReport compares the reassembled frame counts of both transports.
type CompareReport struct {
	TCPFrames int    `json:"tcp_frames"`
	UDPFrames int    `json:"udp_frames"`
	Delta     int    `json:"delta"`
	Verdict   string `json:"verdict"`
}

// NewCompare builds a comparison and derives its verdict from the frame delta.
func NewCompare(tcp, udp PullReport) CompareReport {
	delta := tcp.FramesReassembled - udp.FramesReassembled
	verdict := "consistent"
	if delta != 0 {
		verdict = "mismatch"
	}
	return CompareReport{
		TCPFrames: tcp.FramesReassembled,
		UDPFrames: udp.FramesReassembled,
		Delta:     delta,
		Verdict:   verdict,
	}
}

// WriteJSON atomically writes an indented JSON document.
func WriteJSON(path string, value any) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".audit-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// PrintPull writes a concise terminal summary.
func PrintPull(w io.Writer, r PullReport) {
	fmt.Fprintf(w,
		"transport=%s state_transitions=%d illegal_transitions=%d frames=%d dropped=%d keyframes=%d bytes=%d duplicate_packets=%d late_dropped=%d gaps=%d hashes=%v\n",
		r.Transport, r.StateTransitions, r.IllegalTransitions, r.FramesReassembled,
		r.DroppedFrames, r.Keyframes, r.Bytes, r.DuplicatePackets,
		r.LateDropped, len(r.RTPSeqGaps), r.FrameHashes)
}

// PrintCompare writes a concise terminal comparison.
func PrintCompare(w io.Writer, r CompareReport) {
	fmt.Fprintf(w, "tcp_frames=%d udp_frames=%d delta=%d verdict=%s\n",
		r.TCPFrames, r.UDPFrames, r.Delta, r.Verdict)
}
