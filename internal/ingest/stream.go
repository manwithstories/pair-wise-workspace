// Package ingest turns wire-format RTP into validated, ordered packets on
// the unified clock. Bandwidth estimates adjust the receive batch size and
// the in-memory backlog cap; when the backlog exceeds the cap the oldest
// confirmed packets are spilled to disk, never discarded.
package ingest

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"mediarun/internal/clock"
	"mediarun/internal/model"
	rtporder "mediarun/internal/rtp"
)

// Stream is the receive side of one track. It is safe for concurrent use by
// one producer and one consumer.
type Stream struct {
	cfg     Config
	base    *clock.Base
	reorder *rtporder.Reorder
	spill   spillQueue

	mu         sync.Mutex
	backlog    []model.Event
	pending    []model.Gap
	lastEnd    clock.Tick
	haveLast   bool
	batch      int
	backlogCap int
	stats      Stats
	closed     bool
	spillErr   error
}

// ErrClosed is returned when a closed stream is used again.
var ErrClosed = errors.New("ingest: stream closed")

// Stats are cumulative counters for one stream.
type Stats struct {
	Received   int
	Accepted   int
	Delivered  int
	Duplicates int
	TooOld     int
	Gaps       int
	GapPackets int
	Spilled    int
	Replayed   int
	Dropped    int
}

// New validates cfg and prepares a stream.
func New(cfg Config) (*Stream, error) {
	if cfg.TrackID == 0 {
		return nil, fmt.Errorf("%w: track id required", model.ErrInvalid)
	}
	base, err := clock.NewBase(cfg.Timescale)
	if err != nil {
		return nil, err
	}
	if cfg.Window == 0 {
		cfg.Window = 64
	}
	ro, err := rtporder.New(cfg.TrackID, cfg.Window)
	if err != nil {
		return nil, err
	}
	if cfg.MaxBacklog <= 0 {
		cfg.MaxBacklog = 1024
	}
	if cfg.SpillLimit <= 0 {
		cfg.SpillLimit = 1 << 20
	}
	if cfg.SpillDir == "" {
		cfg.SpillDir = filepath.Join(".", ".mediarun-spill")
	}
	return &Stream{
		cfg:        cfg,
		base:       base,
		reorder:    ro,
		batch:      MinBatch,
		backlogCap: cfg.MaxBacklog,
	}, nil
}
