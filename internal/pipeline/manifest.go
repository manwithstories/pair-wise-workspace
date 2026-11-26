package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	"avpipe/internal/checkpoint"
	"avpipe/internal/hls"
)

const ConfigFile = "pipeline-config.json"
const ReportFile = "report.json"
const ProvenanceFile = "provenance.json"

func segmentRelPath(rendition string, seq int) string {
	return filepath.ToSlash(filepath.Join("segments", rendition, fmtSegName(seq)))
}

func fmtSegName(seq int) string {
	return "seg_" + strconv.Itoa(seq) + ".ts"
}

// BuildManifestInput preserves every slot sequence. Gap slots receive an
// explicit EXT-X-GAP view and no claimed SHA-256.
func BuildManifestInput(rep *Report, idx *checkpoint.Index) hls.BuildInput {
	in := hls.BuildInput{PartCount: 2, Diagnostics: map[string]string{
		"psi_crc_reject": strconv.Itoa(rep.PSICRCReject),
		"missing_idr":    strconv.Itoa(rep.KeyframeExpected - rep.KeyframeFound),
		"gap_slots":      strconv.Itoa(rep.GapSlots),
		"cc_gap":         strconv.Itoa(rep.CCGap),
		"parser_version": rep.ParserVersion,
		"source_hash":    rep.SourceHash,
		"config_hash":    rep.ConfigHash,
	}}
	for _, a := range rep.Analyses {
		rv := hls.RenditionView{
			ID: a.Source.Rendition, Name: a.Source.Rendition,
			Bandwidth: a.Source.Bitrate, PlaylistFile: a.Source.Rendition + ".m3u8",
		}
		for _, slot := range rep.Ladder.Slots {
			rel := segmentRelPath(a.Source.Rendition, slot.Seq)
			sv := hls.SegmentView{Seq: slot.Seq, File: rel, Duration: 1.0}
			var boundaryPresent bool
			var reason string
			for _, b := range slot.Boundaries {
				if b.Rendition == a.Source.Rendition {
					boundaryPresent = b.Present
					if len(b.Reasons) > 0 {
						reason = string(b.Reasons[0])
					}
					break
				}
			}
			if !boundaryPresent || reason != "" || slot.Gap {
				// If another rendition is bad but this boundary is clean, do
				// not mark this rendition's media segment as gap.
				if boundaryPresent && reason == "" {
					if rec := findSegment(idx, slot.Seq, a.Source.Rendition); rec != nil {
						sv.Size = rec.Size
						sv.SHA256 = rec.SHA256
					}
				} else {
					sv.Gap = true
					sv.Reason = reason
				}
			} else if rec := findSegment(idx, slot.Seq, a.Source.Rendition); rec != nil {
				sv.Size = rec.Size
				sv.SHA256 = rec.SHA256
			}
			rv.Segments = append(rv.Segments, sv)
		}
		in.Renditions = append(in.Renditions, rv)
	}
	return in
}

func findSegment(idx *checkpoint.Index, seq int, rendition string) *checkpoint.SegmentRecord {
	if idx == nil || idx.Chunks[seq] == nil {
		return nil
	}
	for i := range idx.Chunks[seq].Segments {
		s := &idx.Chunks[seq].Segments[i]
		if s.Rendition == rendition {
			return s
		}
	}
	return nil
}

func WriteManifests(out string, rep *Report, idx *checkpoint.Index) (hls.RoundTripResult, error) {
	in := BuildManifestInput(rep, idx)
	files, err := hls.Generate(in)
	if err != nil {
		return hls.RoundTripResult{}, err
	}
	if err := hls.WriteFiles(out, files); err != nil {
		return hls.RoundTripResult{}, err
	}
	rt, err := hls.VerifyRoundTrip(files.Master, in)
	if err != nil {
		return rt, err
	}
	for _, rv := range in.Renditions {
		data := files.Media[rv.PlaylistFile]
		checked, matched, err := hls.VerifyMedia(data, rv)
		if err != nil {
			return rt, err
		}
		rt.Checked += checked
		rt.Matched += matched
	}
	rep.RoundTripChecked = rt.Checked
	rep.RoundTripMatched = rt.Matched
	return rt, nil
}

func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
