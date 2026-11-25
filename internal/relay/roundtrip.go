package relay

import (
	"fmt"
	"os"
	"strings"

	"relay.local/relay/internal/audit"
	"relay.local/relay/internal/container"
	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/metadata"
	"relay.local/relay/internal/model"
	"relay.local/relay/internal/scene"
)

func guardOutput(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0o755)
		}
		return fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "frame_") && strings.HasSuffix(name, ".png") {
			continue
		}
		if name == "SHA256SUMS" || name == "checkpoint.json" {
			continue
		}
		return fault.New(-1, 0, fault.KindLimit, false,
			"output directory %s contains %q which this tool did not create", dir, name)
	}
	return nil
}

// shardsFor splits per-frame PNG digests into contiguous segments and exposes
// them in the audit shard shape for reporting.
func shardsFor(frameHashes []string, segments int) []audit.Shard {
	segs := audit.SplitSegments(frameHashes, segments)
	out := make([]audit.Shard, 0, len(segs))
	for _, s := range segs {
		out = append(out, audit.Shard{Index: s.Index, Hash: s.Hash, Bytes: int64(s.Last - s.First + 1)})
	}
	return out
}

// roundTrip rewrites every parsed container and compares field tables.
func roundTrip(sc *scene.Scenario, rep *Report) error {
	var compared, matched int
	for _, clip := range []scene.Clip{sc.MP4, sc.MXF} {
		if clip.File == nil {
			continue
		}
		// Resolve first so the writer emits the effective description rather
		// than raw claims.
		metadata.ResolveAll(clip.File.Tracks, metadata.Options{})
		var out []byte
		var err error
		switch clip.File.Kind {
		case model.MP4:
			out, err = container.WriteMP4(clip.File)
		case model.MXF:
			out, err = container.WriteMXF(clip.File)
		default:
			return fault.New(-1, 0, fault.KindUnsupported, false, "no writer for %s", clip.File.Kind)
		}
		if err != nil {
			return err
		}
		var reparsed *container.File
		if clip.File.Kind == model.MP4 {
			reparsed, err = container.ParseMP4(clip.Name, out, container.DefaultLimits())
		} else {
			reparsed, err = container.ParseMXF(clip.Name, out, container.DefaultLimits())
		}
		if err != nil {
			return fmt.Errorf("relay: rewritten %s failed to parse: %w", clip.Name, err)
		}
		// Resolve the rewritten side identically so the comparison is about
		// container metadata, not resolver state.
		metadata.ResolveAll(reparsed.Tracks, metadata.Options{})
		for i, orig := range clip.File.Tracks {
			if i >= len(reparsed.Tracks) {
				break
			}
			rt := audit.CompareFields(orig.FieldTable(), reparsed.Tracks[i].FieldTable())
			compared += rt.Compared
			matched += rt.Matched
		}
	}
	rep.RoundtripRate = audit.FormatRate(matched, compared)
	if compared != matched {
		rep.Notes = append(rep.Notes, fmt.Sprintf("round-trip field mismatch (%d of %d differ)",
			compared-matched, compared))
	}
	return nil
}

// checkCorruption requires both damaged inputs to fail with an offset.
func checkCorruption(sc *scene.Scenario, rep *Report) error {
	if sc.Corrupt.TruncatedBox != nil {
		_, err := container.ParseMP4("truncated.mp4", sc.Corrupt.TruncatedBox, container.DefaultLimits())
		fe, ok := fault.As(err)
		if !ok || fe.Kind != fault.KindTruncated || fe.Offset < 0 {
			return fault.New(-1, 0, fault.KindCorrupt, false,
				"truncated MP4 was not reported with an offset (got %v)", err)
		}
		rep.Notes = append(rep.Notes, fmt.Sprintf("corrupt_mp4_offset=%d", fe.Offset))
	}
	if sc.Corrupt.BoundsKLV != nil {
		_, err := container.ParseMXF("oversize.mxf", sc.Corrupt.BoundsKLV, container.DefaultLimits())
		fe, ok := fault.As(err)
		if !ok || (fe.Kind != fault.KindLimit && fe.Kind != fault.KindTruncated) || fe.Offset < 0 {
			return fault.New(-1, 0, fault.KindCorrupt, false,
				"out-of-range KLV was not reported with an offset (got %v)", err)
		}
		rep.Notes = append(rep.Notes, fmt.Sprintf("corrupt_mxf_offset=%d", fe.Offset))
	}
	return nil
}

// streamResult pairs a recovered transport stream with the generator's ground
// truth so recovery can be judged rather than merely counted.
