package trackmap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"mediad/internal/bmff"
	"mediad/internal/model"
)

// Request is one explicitly specified source-track to output-track mapping.
type Request struct {
	SourceID        uint32 `json:"source_id"`
	OutputID        uint32 `json:"output_id"`
	OutputTimescale uint32 `json:"output_timescale,omitempty"`
}

type PlannedSample struct {
	SourceIndex int            `json:"source_index"`
	Offset      int64          `json:"offset"`
	Size        uint32         `json:"size"`
	Duration    uint32         `json:"duration"`
	DurationOut uint32         `json:"duration_out"`
	Sync        bool           `json:"sync"`
	DTS         int64          `json:"dts"`
	PTS         int64          `json:"pts"`
	DTSTime     model.Rational `json:"dts_time"`
	PTSTime     model.Rational `json:"pts_time"`
}

type PlannedTrack struct {
	SourceID        uint32          `json:"source_id"`
	OutputID        uint32          `json:"output_id"`
	Handler         string          `json:"handler"`
	Codec           string          `json:"codec"`
	SourceTimescale uint32          `json:"source_timescale"`
	OutputTimescale uint32          `json:"output_timescale"`
	SampleEntry     []byte          `json:"-"`
	Edits           []bmff.Edit     `json:"edits,omitempty"`
	Samples         []PlannedSample `json:"samples"`
}

type Plan struct {
	Digest string         `json:"digest"`
	Tracks []PlannedTrack `json:"tracks"`
}

// Digest returns a stable SHA-256 for the explicit mapping requests.
func Digest(requests []Request) (string, error) {
	canonical := make([]Request, len(requests))
	copy(canonical, requests)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].SourceID != canonical[j].SourceID {
			return canonical[i].SourceID < canonical[j].SourceID
		}
		return canonical[i].OutputID < canonical[j].OutputID
	})
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", model.Wrap(model.CodeInternal, model.StageMap, "encode mapping", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func BuildPlan(idx *bmff.Index, requests []Request) (*Plan, error) {
	if idx == nil {
		return nil, model.New(model.CodeInvalidArgument, model.StageMap, "nil index")
	}
	if len(requests) == 0 {
		return nil, model.New(model.CodeAmbiguousMap, model.StageMap, "at least one explicit track mapping is required")
	}

	seenSource := map[uint32]uint32{}
	seenOutput := map[uint32]uint32{}
	plan := &Plan{}
	for _, req := range requests {
		if req.SourceID == 0 || req.OutputID == 0 {
			return nil, model.New(model.CodeAmbiguousMap, model.StageMap, "source and output track IDs must be non-zero")
		}
		if out, ok := seenSource[req.SourceID]; ok {
			return nil, model.Newf(model.CodeAmbiguousMap, model.StageMap,
				"source track %d mapped more than once (outputs %d and %d)", req.SourceID, out, req.OutputID).
				WithTrack(req.SourceID)
		}
		if src, ok := seenOutput[req.OutputID]; ok {
			return nil, model.Newf(model.CodeAmbiguousMap, model.StageMap,
				"output track %d mapped from both %d and %d", req.OutputID, src, req.SourceID).
				WithTrack(req.SourceID)
		}
		seenSource[req.SourceID] = req.OutputID
		seenOutput[req.OutputID] = req.SourceID

		src, ok := idx.Track(req.SourceID)
		if !ok {
			return nil, model.Newf(model.CodeAmbiguousMap, model.StageMap, "source track %d not found", req.SourceID).
				WithTrack(req.SourceID)
		}
		if src.Encrypted {
			return nil, model.Newf(model.CodeEncryptedTrack, model.StageMap,
				"track %d uses unsupported encryption scheme %q", req.SourceID, src.EncryptionScheme).
				WithTrack(req.SourceID)
		}
		outScale := req.OutputTimescale
		if outScale == 0 {
			outScale = src.Timescale
		}
		pt, err := planTrack(src, req.OutputID, outScale)
		if err != nil {
			return nil, err
		}
		plan.Tracks = append(plan.Tracks, *pt)
	}
	var err error
	plan.Digest, err = Digest(requests)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func planTrack(src *bmff.Track, outputID, outScale uint32) (*PlannedTrack, error) {
	pt := &PlannedTrack{
		SourceID:        src.ID,
		OutputID:        outputID,
		Handler:         src.Handler,
		Codec:           src.Codec,
		SourceTimescale: src.Timescale,
		OutputTimescale: outScale,
		SampleEntry:     src.SampleEntry,
		Edits:           src.Edits,
	}
	var lastDTS *int64
	for _, s := range src.Samples {
		if lastDTS != nil && s.DTS < *lastDTS {
			return nil, model.Newf(model.CodeTimestampBackward, model.StageMap,
				"track %d DTS regresses at sample %d: %d < %d", src.ID, s.Index, s.DTS, *lastDTS).
				WithTrack(src.ID)
		}
		lastDTS = &s.DTS
		dtsTime := model.Ticks(s.DTS, src.Timescale)
		ptsTime := model.Ticks(s.PTS, src.Timescale)
		dts, ok := dtsTime.ExactTicks(outScale)
		if !ok {
			return nil, model.Newf(model.CodeUnsupported, model.StageMap,
				"DTS %s cannot be represented exactly in output timescale %d", dtsTime, outScale).
				WithTrack(src.ID)
		}
		pts, ok := ptsTime.ExactTicks(outScale)
		if !ok {
			return nil, model.Newf(model.CodeUnsupported, model.StageMap,
				"PTS %s cannot be represented exactly in output timescale %d", ptsTime, outScale).
				WithTrack(src.ID)
		}
		durTime := model.Ticks(int64(s.Duration), src.Timescale)
		durTicks, ok := durTime.ExactTicks(outScale)
		if !ok || durTicks < 0 || durTicks > int64(^uint32(0)) {
			return nil, model.Newf(model.CodeUnsupported, model.StageMap,
				"duration %s cannot be represented exactly as uint32 at timescale %d", durTime, outScale).
				WithTrack(src.ID)
		}
		pt.Samples = append(pt.Samples, PlannedSample{
			SourceIndex: s.Index, Offset: s.Offset, Size: s.Size, Duration: s.Duration,
			DurationOut: uint32(durTicks), Sync: s.Sync, DTS: dts, PTS: pts, DTSTime: dtsTime, PTSTime: ptsTime,
		})
	}
	return pt, nil
}
