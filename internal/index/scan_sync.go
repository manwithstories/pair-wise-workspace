package index

import (
	"io"

	"mediad/internal/media"
)

// RebuildVerified recovers missing stss entries by scanning only each sample's
// fixed sync marker prefix, then verifies every generated seek point.
func RebuildVerified(p *media.Project, r io.ReadSeeker, fileSize int64) (*Index, []*media.Fault) {
	var faults []*media.Fault
	for _, t := range p.Tracks {
		if t == nil || hasSyncSample(t) {
			continue
		}
		for i := range t.Samples {
			s := &t.Samples[i]
			if s.Offset < 0 || int64(s.Size) < int64(len(SyncMagic)) {
				continue
			}
			end, ok := media.ByteRange{Offset: s.Offset, Length: int64(s.Size)}.End()
			if !ok || end > fileSize {
				continue
			}
			if _, err := r.Seek(s.Offset, io.SeekStart); err != nil {
				continue
			}
			magic := make([]byte, len(SyncMagic))
			if _, err := io.ReadFull(r, magic); err != nil {
				continue
			}
			if string(magic) == SyncMagic {
				s.Sync = true
			}
		}
		if !hasSyncSample(t) {
			f := media.NewFault(media.CodeMissingIndex, "stss missing and no sync markers found").
				WithTrack(t.ID).WithBox("stss").Fatal()
			faults = append(faults, f)
		} else {
			f := media.NewFault(media.CodeMissingIndex, "rebuilt missing stss from verified sync markers").
				WithTrack(t.ID).WithBox("stss").WithBasis("sample sync marker scan")
			t.Faults = append(t.Faults, f)
			p.Faults = append(p.Faults, f)
		}
	}
	idx, rebuildFaults := Rebuild(p)
	faults = append(faults, rebuildFaults...)
	misses, verifyFaults := idx.Verify(r, fileSize)
	faults = append(faults, verifyFaults...)
	idx.Misses = misses
	return idx, faults
}

func hasSyncSample(t *media.Track) bool {
	for _, s := range t.Samples {
		if s.Sync {
			return true
		}
	}
	return false
}
