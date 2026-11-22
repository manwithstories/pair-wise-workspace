package fixture

import (
	"bytes"
	"io"
	"testing"

	"mediapack/internal/clock"
	"mediapack/internal/ebml"
)

// stats accumulates structural counts while recursively walking scopes.
type stats struct {
	clusters int
	blocks   int
}

// parseStats walks a fixture with the production parser and counts
// structural elements, returning the first error encountered. The walk
// is one recursive function so EOF/scope handling lives in one place.
func parseStats(t *testing.T, data []byte) (clusters, blocks int, err error) {
	t.Helper()
	r := ebml.NewReader(bytes.NewReader(data), 1<<20)
	s := &stats{}
	err = walk(r, s, -1)
	return s.clusters, s.blocks, err
}

// walk consumes elements at the current depth. parentID is the pushed
// container's ID (-1 at top level). On EOF the current scope (if any)
// is closed and the function returns to its caller.
func walk(r *ebml.Reader, s *stats, parentID int64) error {
	for {
		el, err := r.Next()
		if err == io.EOF {
			if r.Depth() > 0 {
				r.CloseCurrent()
			}
			return nil
		}
		if err != nil {
			if r.Depth() > 0 {
				r.CloseCurrent()
			}
			return err
		}
		switch {
		case el.ID == ebml.IDSegment || el.ID == ebml.IDCluster:
			if el.ID == ebml.IDCluster {
				s.clusters++
			}
			r.Push(el)
			if err := walk(r, s, int64(el.ID)); err != nil {
				return err
			}
		case el.ID == ebml.IDSimpleBlock || el.ID == ebml.IDBlock:
			body, err := r.ReadBody(el, 1<<16)
			if err != nil {
				return err
			}
			blk, err := ebml.DecodeBlock(body, el.HeaderOff, el.ID == ebml.IDSimpleBlock)
			if err != nil {
				return err
			}
			blk.SetPayloadOff(el.BodyOff, 0)
			if blk.Track == 0 {
				return clock.NewError(clock.CatBadBlock, "zero track at %d", el.HeaderOff)
			}
			s.blocks++
		case el.Unknown:
			// Unknown non-container element: nothing to read; close it.
			r.CloseCurrent()
		default:
			if err := r.DiscardBody(el); err != nil {
				return err
			}
		}
	}
}
