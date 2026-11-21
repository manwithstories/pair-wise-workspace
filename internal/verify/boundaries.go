package verify

import (
	"errors"
	"io"
	"os"

	"github.com/mediamend/mediamend/internal/clock"
	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/frame"
	"github.com/mediamend/mediamend/internal/rtp"
)

// BoundaryPaths names the four sampled defect fixtures.
type BoundaryPaths struct {
	Truncated        string
	ChecksumMismatch string
	TimestampWrap    string
	Duplicate        string
}

// BoundaryReport is the self-check outcome. The four counts are 1 when the
// corresponding defect was detected, which is what the acceptance run prints.
type BoundaryReport struct {
	Truncated        int        `json:"truncated"`
	ChecksumMismatch int        `json:"checksum_mismatch"`
	TimestampWrap    int        `json:"timestamp_wrap"`
	Duplicate        int        `json:"duplicate"`
	TruncatedOffset  int64      `json:"truncated_offset"`
	ChecksumOffset   int64      `json:"checksum_offset"`
	Faults           fault.List `json:"faults"`
}

// OK reports whether every defect sample was detected.
func (r *BoundaryReport) OK() bool {
	return r.Truncated == 1 && r.ChecksumMismatch == 1 &&
		r.TimestampWrap == 1 && r.Duplicate == 1 && len(r.Faults) == 0
}

// SelfCheckBoundaries parses each boundary fixture and counts the defects the
// pipeline would locate. Missing files are reported as faults rather than
// silently passing.
func SelfCheckBoundaries(p BoundaryPaths) *BoundaryReport {
	rep := &BoundaryReport{TruncatedOffset: -1, ChecksumOffset: -1}

	// 1. Truncated record: the reader must stop with a located fault.
	if recs, fs, ok := readFixture(p.Truncated, "check"); ok {
		if f, found := fs.FirstOf(fault.KindTruncatedRecord); found {
			rep.Truncated = 1
			rep.TruncatedOffset = f.Offset
		} else {
			rep.Faults = append(rep.Faults, fault.Fault{
				Kind: fault.KindTruncatedRecord, Stage: fault.StageVerify,
				File: p.Truncated, Message: "truncation sample did not produce a truncated_record fault",
			})
		}
		// Everything before the truncation must still be readable.
		if len(recs) == 0 {
			rep.Faults = append(rep.Faults, fault.Fault{
				Kind: fault.KindMalformedMetadata, Stage: fault.StageVerify,
				File: p.Truncated, Message: "no records readable before the truncation",
			})
		}
	}

	// 2. Checksum mismatch: metadata digest must disagree with the payload.
	if md, err := frame.LoadMetadata(metaPathFor(p.ChecksumMismatch)); err == nil {
		recs, _, ok := readFixture(p.ChecksumMismatch, "check")
		if ok {
			for _, rec := range recs {
				e, found := md.LookupOffset(rec.Offset)
				if !found {
					continue
				}
				if verified, f := frame.VerifyPayload(e, rec.Payload, p.ChecksumMismatch); !verified {
					rep.ChecksumMismatch++
					rep.ChecksumOffset = f.Offset
					break
				}
			}
			if rep.ChecksumMismatch == 0 {
				rep.Faults = append(rep.Faults, fault.Fault{
					Kind: fault.KindChecksumMismatch, Stage: fault.StageVerify,
					File: p.ChecksumMismatch, Message: "checksum sample has no digest mismatch",
				})
			}
		}
	} else {
		rep.Faults = append(rep.Faults, fault.Fault{
			Kind: fault.KindMalformedMetadata, Stage: fault.StageVerify,
			File: p.ChecksumMismatch, Message: "checksum sample metadata unreadable",
		})
	}

	// 3. Timestamp wrap: a 32-bit device clock crossing zero must map to a
	// strictly increasing master timeline.
	if recs, _, ok := readFixture(p.TimestampWrap, "check"); ok && len(recs) >= 2 {
		cfg := clock.Config{Camera: "check", Rate: clock.MasterRate, FrameStep: 3000,
			AnchorSeq: recs[0].Seq, AnchorDeviceTS: recs[0].DeviceTS, AnchorMasterTS: 0}
		seqs := make([]uint64, len(recs))
		ts := make([]uint64, len(recs))
		offs := make([]int64, len(recs))
		for i, rec := range recs {
			seqs[i], ts[i], offs[i] = rec.Seq, rec.DeviceTS, rec.Offset
		}
		res, err := clock.MapFrames(cfg, seqs, ts, offs)
		if err != nil {
			rep.Faults = append(rep.Faults, fault.Fault{
				Kind: fault.KindTimestampJump, Stage: fault.StageVerify,
				File: p.TimestampWrap, Message: err.Error(),
			})
		} else {
			if res.Wraps > 0 {
				rep.TimestampWrap = 1
			}
			if i := clock.CheckStrictlyIncreasing(res.Timestamps()); i >= 0 {
				rep.Faults = append(rep.Faults, fault.Fault{
					Kind: fault.KindTimestampJump, Stage: fault.StageVerify,
					File:    p.TimestampWrap,
					Message: "timeline is not strictly increasing after the wrap",
				})
			}
		}
	}

	// 4. Duplicate: the reorderer must suppress the repeated sequence.
	if recs, _, ok := readFixture(p.Duplicate, "check"); ok && len(recs) >= 2 {
		ro, err := rtp.New(rtp.Config{Camera: "check", Window: 8})
		if err == nil {
			for _, rec := range recs {
				ro.Push(rtp.Frame{
					Camera: "check", FrameSeq: rec.Seq, MasterTS: rec.DeviceTS,
					Offset: rec.Offset, RTPSeq: uint16(rec.Seq),
				})
			}
			res := ro.Finish()
			if res.Duplicates > 0 && len(res.Ordered) == 1 {
				rep.Duplicate = 1
			} else {
				rep.Faults = append(rep.Faults, fault.Fault{
					Kind: fault.KindDuplicateFrame, Stage: fault.StageVerify,
					File:    p.Duplicate,
					Message: "duplicate sample was not suppressed to a single frame",
				})
			}
		}
	}

	rep.Faults = rep.Faults.Sorted()
	return rep
}

// metaPathFor maps a frame file to its metadata sidecar.
func metaPathFor(framePath string) string {
	return framePath[:len(framePath)-len(".frm")] + ".meta"
}

// readFixture reads every readable record, returning them plus the faults
// the reader accumulated. ok is false when the file does not exist.
func readFixture(path, camera string) ([]*frame.Record, fault.List, bool) {
	if path == "" {
		return nil, fault.List{{Kind: fault.KindMalformedMetadata, Stage: fault.StageVerify,
			File: path, Message: "boundary fixture path is empty"}}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fault.List{{Kind: fault.KindMalformedMetadata, Stage: fault.StageVerify,
			File: path, Message: err.Error()}}, false
	}
	defer f.Close()
	r := frame.NewReader(f, camera, 0)
	var recs []*frame.Record
	for {
		// Read aliases the reader buffer, so copy the parts we keep.
		rec, err := r.Read()
		if rec != nil && len(rec.Payload) > 0 {
			cp := *rec
			cp.Payload = append([]byte(nil), rec.Payload...)
			recs = append(recs, &cp)
		}
		if errors.Is(err, frame.ErrBadFraming) {
			return recs, r.Faults(), true
		}
		if err == io.EOF || err != nil {
			return recs, r.Faults(), true
		}
	}
}
