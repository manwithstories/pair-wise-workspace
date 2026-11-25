// Package remux writes compressed packets into a target container without
// decoding or rewriting media data. Both writers expose the same Sink shape so
// the pipeline can pick a target before the first packet is written.
package remux

import (
	"crypto/sha256"
	"hash"
	"io"
	"sort"

	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
)

// Sink accepts packets in delivery order and produces a container.
type Sink interface {
	// WritePacket appends one packet. Packets must be supplied in ascending
	// DTS per output track; the sink buffers only what clustering requires.
	WritePacket(p media.Packet) error
	// Flush completes every buffered cluster/fragment and returns a resumable
	// progress snapshot. Nothing is buffered afterwards.
	Flush() (Progress, error)
	// Close finalises the container and returns the byte count and SHA-256
	// digest of everything written.
	Close() (Summary, error)
}

// Progress is a checkpoint written at a container boundary. Restoring it
// resumes exactly where the previous run stopped, so the final digest matches
// an uninterrupted run.
type Progress struct {
	Bytes    int64
	Packets  int
	Clusters int
	// HashState is the marshalled SHA-256 state of all bytes written so far.
	HashState []byte
	TimeStart media.Timestamp
	TimeEnd   media.Timestamp
	HaveTime  bool
}

// ResumeState is passed through Options to append to a truncated output.
type ResumeState struct {
	Progress
	// TruncateTo is the byte length the output file must be cut back to so it
	// matches Bytes exactly. The sink does not perform the truncation.
	TruncateTo int64
}

// Summary reports what a Sink produced.
type Summary struct {
	Bytes     int64
	Digest    [32]byte
	Packets   int
	Clusters  int
	Target    mapping.Target
	Tracks    []media.Track
	TimeStart media.Timestamp
	TimeEnd   media.Timestamp
}

// Options tunes container writing.
type Options struct {
	Target mapping.Target
	// ClusterSpan groups packets into clusters covering this span. Zero uses
	// a one-second default.
	ClusterSpan media.Timestamp
	// UnknownSegment writes an all-ones size for the Segment header.
	UnknownSegment bool
	// UnknownCluster writes all-ones sizes for Cluster headers.
	UnknownCluster bool
	// ClusterCRC writes EBML CRC-32 elements per cluster.
	ClusterCRC bool
	// Resume, when set, skips the container header and continues the digest
	// from the checkpoint.
	Resume *ResumeState
}

func (o Options) clusterSpan() media.Timestamp {
	if o.ClusterSpan <= 0 {
		return media.Second
	}
	return o.ClusterSpan
}

// NewSink constructs the writer for the requested target.
func NewSink(w io.Writer, plan *mapping.Plan, tracks []media.Track, opts Options) (Sink, error) {
	if plan == nil {
		return nil, media.NewError("remux.sink", media.ClassUnsupported, errNoPlan{})
	}
	switch opts.Target {
	case mapping.TargetMatroska, mapping.TargetWebM:
		return newMKVSink(w, plan, tracks, opts)
	case mapping.TargetFMP4:
		return newMP4Sink(w, plan, tracks, opts)
	default:
		return nil, media.NewError("remux.sink", media.ClassUnsupported, errUnknownTarget{string(opts.Target)})
	}
}

type errNoPlan struct{}

func (errNoPlan) Error() string { return "remux requires a mapping plan" }

type errUnknownTarget struct{ target string }

func (e errUnknownTarget) Error() string { return "unknown remux target " + e.target }

// hasherWriter counts bytes and feeds a hash while ignoring seeks.
type hasherWriter struct {
	w   io.Writer
	n   int64
	h   hash.Hash
	err error
}

func newHasherWriter(w io.Writer) *hasherWriter {
	return &hasherWriter{w: w, h: sha256.New()}
}

func (hw *hasherWriter) Write(p []byte) (int, error) {
	if hw.err != nil {
		return 0, hw.err
	}
	n, err := hw.w.Write(p)
	if n > 0 {
		hw.n += int64(n)
		hw.h.Write(p[:n])
	}
	if err != nil {
		hw.err = err
	}
	return n, err
}

func (hw *hasherWriter) Summary() Summary {
	var d [32]byte
	copy(d[:], hw.h.Sum(nil))
	return Summary{Bytes: hw.n, Digest: d}
}

func (hw *hasherWriter) state() ([]byte, error) {
	state, err := hw.h.(interface{ MarshalBinary() ([]byte, error) }).MarshalBinary()
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (hw *hasherWriter) restore(r *ResumeState) error {
	if len(r.HashState) == 0 {
		return media.NewError("remux.resume", media.ClassInterrupted, errNoHashState{})
	}
	if err := hw.h.(interface{ UnmarshalBinary([]byte) error }).UnmarshalBinary(r.HashState); err != nil {
		return media.NewError("remux.resume", media.ClassInterrupted, errNoHashState{})
	}
	hw.n = r.Bytes
	return nil
}

type errNoHashState struct{}

func (errNoHashState) Error() string { return "checkpoint has no usable SHA-256 state" }

// orderedTracks returns output tracks sorted by OutTrack.
func orderedTracks(plan *mapping.Plan) []mapping.Route {
	routes := plan.OutputTracks()
	sort.SliceStable(routes, func(i, j int) bool { return routes[i].OutTrack < routes[j].OutTrack })
	return routes
}

func planTrackByInput(plan *mapping.Plan, in uint64) (mapping.Route, bool) {
	return plan.Lookup(in)
}
