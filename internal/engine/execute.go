package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"bcast/internal/fault"
	"bcast/internal/index"
	"bcast/internal/model"
	"bcast/internal/provenance"
	"bcast/internal/render"
	"bcast/internal/rewrap"
	"bcast/internal/safe"
	"bcast/internal/timebase"
)

func (e *execConfig) executeSegment(ctx context.Context, seg model.Segment) (*provenance.SegmentManifest, error) {
	frameDir := fmt.Sprintf("frames/%s", seg.Name)
	mp4Rel := fmt.Sprintf("mp4/%s.mp4", seg.Name)
	fps := e.params.FPS
	frameDur := e.timeline.FrameDuration

	var frames []provenance.FrameHash
	outputs := map[string]string{}

	switch seg.Mode {
	case "copy":
		src := seg.Sources[0]
		if err := assertKeyframe(e.indexStore, src.Asset, seg.KeyframeNeed); err != nil {
			return nil, err
		}
		start, count, err := rangeFrames(src, frameDur)
		if err != nil {
			return nil, err
		}
		frames, err = render.ExportRange(ctx, e.root, src.Asset, frameDir, "cut", src.Asset, int(start), int(count))
		if err != nil {
			return nil, err
		}
		res, err := rewrap.CopyCompatible(ctx, e.root, src.Asset, mp4Rel, int(start), int(count), fps)
		if err != nil {
			return nil, err
		}
		outputs["mp4"] = res.PathRel
		outputs["mp4_sha256"] = res.SHA256

	case "composite":
		if len(seg.Sources) != 2 {
			return nil, fault.E(fault.TimelineInvalid, "engine", "executeSegment", fmt.Errorf("composite %d needs two sources", seg.Index)).WithSegment(seg.Index)
		}
		// Export the source frames around the cut for human review.
		for _, src := range seg.Sources {
			start, count, err := rangeFrames(src, frameDur)
			if err != nil {
				return nil, err
			}
			if _, err := render.ExportRange(ctx, e.root, src.Asset, frameDir, "source", src.Asset, int(start), int(count)); err != nil {
				return nil, err
			}
		}

		a, err := decodeSource(ctx, e, seg.Sources[0], frameDur)
		if err != nil {
			return nil, err
		}
		b, err := decodeSource(ctx, e, seg.Sources[1], frameDur)
		if err != nil {
			return nil, err
		}
		n := len(a)
		if len(b) != n || n == 0 {
			return nil, fault.E(fault.RenderFailed, "engine", "executeSegment", fmt.Errorf("dissolve frame counts %d/%d", len(a), len(b))).WithSegment(seg.Index)
		}

		frameCh := make(chan []byte, n)
		for i := 0; i < n; i++ {
			alpha := model.Rational{N: int64(i + 1), D: int64(n + 1)}
			blended, berr := render.Dissolve(a[i].RGB, b[i].RGB, alpha)
			if berr != nil {
				return nil, berr
			}
			rel := fmt.Sprintf("%s/composite-%03d.ppm", frameDir, i)
			path, rerr := e.root.Resolve(rel)
			if rerr != nil {
				return nil, rerr
			}
			if werr := render.WritePPM(path, blended); werr != nil {
				return nil, werr
			}
			pts := timebase.Add(seg.Start, timebase.FrameTime(int64(i), frameDur))
			frames = append(frames, provenance.FrameHash{
				Index: i, PTS: pts, SHA256: provenance.HashBytes(blended),
			})
			frameCh <- blended
		}
		close(frameCh)
		res, err := rewrap.EncodeRawRGB(ctx, e.root, mp4Rel, e.params.Width, e.params.Height, fps, frameCh)
		if err != nil {
			return nil, err
		}
		outputs["mp4"] = res.PathRel
		outputs["mp4_sha256"] = res.SHA256

	default:
		return nil, fault.E(fault.TimelineInvalid, "engine", "executeSegment", fmt.Errorf("unknown mode %q", seg.Mode)).WithSegment(seg.Index)
	}

	m := &provenance.SegmentManifest{
		Segment:     seg.Index,
		Name:        seg.Name,
		Mode:        seg.Mode,
		InputSHA256: e.key.Inputs,
		TimelineSHA: e.key.TimelineSHA,
		ParamsSHA:   e.key.ParamsSHA,
		Sources:     seg.Sources,
		Frames:      frames,
		OutputFiles: outputs,
	}
	sha, err := m.ComputeHash()
	if err != nil {
		return nil, fault.E(fault.Internal, "engine", "executeSegment.hash", err).WithSegment(seg.Index)
	}
	if err := writeManifest(e, m); err != nil {
		return nil, err
	}
	if got := m.SHA256; got != sha {
		return nil, fault.E(fault.EvidenceMismatch, "engine", "executeSegment", fmt.Errorf("segment hash changed"))
	}
	return m, nil
}

type execConfig struct {
	root       *safe.Root
	key        index.Key
	indexStore *index.Store
	timeline   *model.Timeline
	params     Params
}

func assertKeyframe(store *index.Store, assetRel string, need []uint32) error {
	if store == nil {
		return nil
	}
	asset := store.Assets[assetRel]
	if asset == nil || asset.Rejected {
		return fault.E(fault.IndexInvalid, "engine", "assertKeyframe", fmt.Errorf("asset index missing: %s", assetRel)).WithPath(assetRel)
	}
	for _, n := range need {
		hit := false
		for _, tr := range asset.Tracks {
			if int(n) >= 1 && int(n) <= len(tr.Samples) && tr.Samples[n-1].Index == n && tr.Samples[n-1].Keyframe {
				hit = true
				break
			}
		}
		if !hit {
			return fault.E(fault.IndexInvalid, "engine", "assertKeyframe",
				fmt.Errorf("sample %d is not indexed as a keyframe", n)).WithPath(assetRel)
		}
	}
	return nil
}

func rangeFrames(src model.SourceInterval, frameDur model.Rational) (int64, int64, error) {
	start, err := timebase.FrameBoundary(src.Start, frameDur)
	if err != nil {
		return 0, 0, err
	}
	end, err := timebase.FrameBoundary(src.End, frameDur)
	if err != nil {
		return 0, 0, err
	}
	if end <= start {
		return 0, 0, fault.E(fault.TimelineInvalid, "engine", "rangeFrames", fmt.Errorf("empty source range %s", src.Asset))
	}
	return start, end - start, nil
}

func decodeSource(ctx context.Context, e *execConfig, src model.SourceInterval, frameDur model.Rational) ([]render.Frame, error) {
	path, err := e.root.Resolve(src.Asset)
	if err != nil {
		return nil, err
	}
	start, count, err := rangeFrames(src, frameDur)
	if err != nil {
		return nil, err
	}
	var out []render.Frame
	err = render.DecodeRGBRange(ctx, "ffmpeg", path, int(start), int(count), func(fr render.Frame) error {
		out = append(out, fr)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func writeManifest(e *execConfig, m *provenance.SegmentManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fault.E(fault.Internal, "engine", "writeManifest.marshal", err).WithSegment(m.Segment)
	}
	rel := fmt.Sprintf("manifests/%s.json", m.Name)
	tmp := rel + ".tmp"
	if err := e.root.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if _, err := e.root.CommitTemp(tmp, rel); err != nil {
		return err
	}
	if err := e.root.VerifyHash(rel, provenance.HashBytes(b)); err != nil {
		// VerifyHash compares file SHA-256, while b is the pre-rename bytes;
		// rename preserves bytes so any mismatch is a real write fault.
		return err
	}
	return nil
}
