package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/pairwise/relay/internal/cache"
	"github.com/pairwise/relay/internal/checkpoint"
	"github.com/pairwise/relay/internal/isobmff"
	"github.com/pairwise/relay/internal/media"
)

type segmentStore struct {
	parsed   map[string]map[uint64]*isobmff.Fragment
	metas    map[string]map[uint64]cache.Meta
	MFHDGaps int
}

func processSegments(
	ctx context.Context,
	in Input,
	cas *cache.Cache,
	cp *checkpoint.Checkpoint,
	cpPath string,
	trs []*resolvedRung,
) (*segmentStore, bool, error) {
	store := &segmentStore{
		parsed: make(map[string]map[uint64]*isobmff.Fragment, len(trs)),
		metas:  make(map[string]map[uint64]cache.Meta, len(trs)),
	}
	lastMFHD := make(map[string]uint32)
	haveMFHD := make(map[string]bool)
	verifiedUntil := cp.ProcessedSegments

	for seg := uint64(1); seg <= uint64(in.Segments); seg++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		digestParts := make([]string, 0, len(trs))
		for _, rr := range trs {
			id := rr.input.ID
			if store.parsed[id] == nil {
				store.parsed[id] = make(map[uint64]*isobmff.Fragment)
				store.metas[id] = make(map[uint64]cache.Meta)
			}
			if rr.gaps[seg] || rr.data[seg] == nil {
				digestParts = append(digestParts, "gap")
				continue
			}

			source := rr.source[seg]
			if source == "" {
				source = rr.input.SourceNode
			}
			key := cache.Key{SourceNode: source, StreamID: in.StreamID, Rendition: id, Segment: seg}
			data := rr.data[seg]
			got, meta, err := cas.Get(key)
			if err != nil {
				if f, ok := media.AsFault(err); !ok || !f.Recoverable {
					return nil, false, media.NewExitError(4, err)
				}
				meta, err = cas.Put(key, data)
				if err != nil {
					return nil, false, err
				}
			} else {
				data = got
			}
			store.metas[id][seg] = meta
			digestParts = append(digestParts, meta.SHA256)

			frag, f := isobmff.ParseFragment(data, media.FragmentID{Rendition: id, Segment: seg})
			if f != nil {
				// A malformed transport copy may have an intact refetch copy.
				if intact := rr.input.Intact[seg]; intact != nil && string(intact) != string(data) {
					if refetched, f2 := isobmff.ParseFragment(intact, media.FragmentID{Rendition: id, Segment: seg}); f2 == nil {
						putMeta, perr := cas.Put(key, intact)
						if perr != nil {
							return nil, false, perr
						}
						meta = putMeta
						frag = refetched
						f = nil
					}
				}
			}
			if f != nil {
				// Isolate the bad fragment but keep the common grid moving.
				rr.gaps[seg] = true
				rr.gapSegments = append(rr.gapSegments, seg)
				digestParts[len(digestParts)-1] = "isolated:" + checkpoint.HashBytes([]byte(f.Error()))
				continue
			}
			store.parsed[id][seg] = frag

			prevParsed := seg > 1 && store.parsed[id][seg-1] != nil
			if prevParsed && frag.MFHDSequence != lastMFHD[id]+1 {
				// A jump between two adjacent parsed logical segments is an
				// actual mfhd numbering gap. Jumps across an unavailable
				// segment are already accounted for as transport gaps.
				store.MFHDGaps++
			}
			lastMFHD[id] = frag.MFHDSequence
			haveMFHD[id] = true
		}

		stageName := segmentStageName(seg)
		segmentDigest := checkpoint.HashBytes([]byte(strings.Join(digestParts, "\n")))
		if int(seg) <= verifiedUntil {
			prior, ok := cp.Stage(stageName)
			if !ok || !prior.Completed || prior.Digest != segmentDigest {
				return nil, false, media.NewExitError(4, media.NewFault(media.CodeCheckpoint,
					media.FragmentID{Segment: seg}, -1, false,
					"completed stage %s failed revalidation", stageName))
			}
		} else {
			cp.SetStage(stageName, segmentDigest, true)
			if int(seg) > cp.ProcessedSegments {
				cp.ProcessedSegments = int(seg)
			}
			// Keep the in-memory checkpoint complete for every segment, but
			// persist periodically and always at a simulated interrupt.
			shouldSave := seg%4 == 0 ||
				(in.InterruptAfter == int(seg) && !in.Resume) ||
				int(seg) == in.Segments
			if shouldSave {
				if err := checkpoint.Save(cpPath, cp); err != nil {
					return nil, false, err
				}
			}
		}

		if in.InterruptAfter == int(seg) && !in.Resume {
			if err := checkpoint.Save(cpPath, cp); err != nil {
				return nil, false, err
			}
			return store, true, nil
		}
	}
	return store, false, nil
}

func segmentStageName(seg uint64) string {
	return fmt.Sprintf("segment-%06d", seg)
}

func verifyDamage(in Input) (int, int64, []*media.Fault) {
	checked := 0
	var maxDelta int64
	var faults []*media.Fault
	for _, d := range in.Damage {
		_, f := isobmff.ParseTopBoxes(d.Data, media.FragmentID{Rendition: d.Rendition, Segment: d.Segment})
		if f == nil {
			f = media.NewFault(media.CodeOK, media.FragmentID{Rendition: d.Rendition, Segment: d.Segment}, -1, false, "damage sample was accepted")
		}
		checked++
		faults = append(faults, f)
		if string(f.Code) != d.WantCode {
			delta := int64(1 << 40)
			maxDelta += delta
			continue
		}
		delta := f.Offset - d.WantOffset
		if delta < 0 {
			delta = -delta
		}
		if delta > maxDelta {
			maxDelta = delta
		}
	}
	return checked, maxDelta, faults
}
