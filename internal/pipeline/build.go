package pipeline

import (
	"os"
	"path/filepath"
	"sort"

	"mediad/internal/core"
	"mediad/internal/manifest"
	"mediad/internal/metadata"
	"mediad/internal/mp4"
	"mediad/internal/source"
	"mediad/internal/timeline"
)

// Report is the outcome of an execute/resume run.
type Report struct {
	ExitCode        int
	Canceled        bool
	Committed       int
	Resumed         int
	DuplicateEmit   int
	InputDigest     string
	ConfigDigest    string
	ResultHash      string
	ExpectedHash    string
	ResultHashEqual bool
	Manifest        *manifest.Manifest
	Verify          manifest.VerifyReport
}

type repMeta struct {
	timescale uint32
	desc      metadata.Descriptor
}

func loadMetadata(plan *source.Plan) map[string]repMeta {
	out := map[string]repMeta{}
	for _, in := range plan.Inits {
		b, err := os.ReadFile(in.Abs)
		if err != nil {
			continue
		}
		info, err := mp4.ParseInit(b)
		if err != nil {
			continue
		}
		cur := out[in.Representation]
		cur.desc.Representation = in.Representation
		if v, ok := info.Video(); ok {
			if v.Timescale != 0 {
				cur.timescale = v.Timescale
			}
			if v.Color != nil {
				cur.desc.Color = v.Color
			}
		}
		if a, ok := info.Audio(); ok && a.Audio != nil {
			cur.desc.Audio = a.Audio
		}
		if cur.timescale == 0 {
			cur.timescale = 90000
		}
		out[in.Representation] = cur
	}
	return out
}

func countKeyframes(f *timeline.Fragment) int {
	n := 0
	for _, s := range f.Samples {
		if s.Sync {
			n++
		}
	}
	return n
}

func prefixed(prefix, rel string) string {
	if prefix == "" {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Join(prefix, filepath.FromSlash(rel)))
}

// buildManifest runs the pure deterministic stages over committed items.
func buildManifest(cp *Checkpoint, plan *source.Plan, meta map[string]repMeta, caseName string, seed int64, pathPrefix string, cfg Config) (*manifest.Manifest, string, timeline.AlignmentReport) {
	obs := make([]timeline.Observation, 0, len(cp.Items))
	rtOK := map[string]bool{}
	for _, it := range cp.Items {
		obs = append(obs, it.Obs)
		rtOK[it.Rel] = it.RoundTripOK
	}
	tr := timeline.Repair(obs)

	srcByRel := map[string]source.Source{}
	repsSet := map[string]bool{}
	for _, s := range plan.Fragments {
		srcByRel[s.Rel] = s
		repsSet[s.Representation] = true
	}
	reps := make([]string, 0, len(repsSet))
	for r := range repsSet {
		reps = append(reps, r)
	}
	sort.Strings(reps)

	descs := make([]metadata.Descriptor, 0, len(meta))
	for _, r := range reps {
		if m, ok := meta[r]; ok {
			descs = append(descs, m.desc)
		}
	}
	audioGroups := metadata.GroupAudio(descs)
	video := metadata.CheckVideo(descs)
	incompat := map[string]bool{}
	for _, r := range video.Incompatible {
		incompat[r] = true
	}

	records := make([]manifest.FragmentRecord, 0, len(tr.Fragments))
	bounds := map[string]*timeline.RepBoundaries{}
	for _, f := range tr.Fragments {
		src := srcByRel[f.Path]
		m := meta[f.Representation]
		kf := countKeyframes(f)
		rec := manifest.FragmentRecord{
			ID: f.Path, Path: prefixed(pathPrefix, f.Path), SHA256: f.Digest, Size: src.Size,
			Camera: f.Camera, Representation: f.Representation, Sequence: f.Sequence,
			Period: f.Period, Timescale: f.Timescale, TFDT: f.TFDT,
			StartDTS: f.StartDTS, EndDTS: f.EndDTS, StartPTS90k: f.StartPTS90k,
			SampleCount: len(f.Samples), KeyframeCount: kf,
			RoundTripOK: rtOK[f.Path], RTPRepaired: f.RTPRepaired,
		}
		if m.desc.Audio != nil {
			rec.Audio = m.desc.Audio
		}
		if m.desc.Color != nil {
			rec.Color = m.desc.Color
		}
		records = append(records, rec)

		if kf > 0 && f.Period == 0 && f.TimingValid {
			rb := bounds[f.Representation]
			if rb == nil {
				frame90 := timeline.DefaultFrame90k
				if len(f.Samples) > 0 && f.Timescale != 0 {
					if v, ok := core.ScaleTicksFloor(int64(f.Samples[0].Duration), f.Timescale, timeline.CommonTimescale); ok && v > 0 {
						frame90 = v
					}
				}
				rb = &timeline.RepBoundaries{
					Representation: f.Representation, FrameDur90k: frame90,
					Eligible: !incompat[f.Representation],
				}
				bounds[f.Representation] = rb
			}
			rb.Boundaries = append(rb.Boundaries, timeline.KeyBoundary{
				PTS90k: f.StartPTS90k, FragmentID: f.Path, Sequence: f.Sequence,
			})
		}
	}

	repBounds := make([]timeline.RepBoundaries, 0, len(bounds))
	for _, rb := range bounds {
		repBounds = append(repBounds, *rb)
	}
	align := timeline.Align(repBounds)

	points := make([]manifest.SwitchPoint, 0, len(align.Points))
	for _, p := range align.Points {
		mp := manifest.SwitchPoint{
			PTS90k: p.PTS90k, Switchable: p.Switchable,
			MaxDeviation90k: p.MaxDeviation90k, Reason: p.Reason,
		}
		for _, mem := range p.Members {
			mp.Members = append(mp.Members, manifest.SwitchMember{
				Representation: mem.Representation, FragmentID: mem.FragmentID,
				PTS90k: mem.PTS90k, Deviation90k: mem.Deviation90k, WithinFrame: mem.WithinFrame,
			})
		}
		points = append(points, mp)
	}

	gaps, ooo, dupSeq, _, _, _, _ := tr.Counters()
	rtOKN, rtTotal := 0, len(cp.Items)
	kfHits := 0
	for _, it := range cp.Items {
		if it.RoundTripOK {
			rtOKN++
		}
	}
	for _, r := range records {
		if r.KeyframeCount > 0 {
			kfHits++
		}
	}

	mf := &manifest.Manifest{
		Schema: manifest.SchemaVersion, Case: caseName, Seed: seed,
		InputDigest: cp.InputDigest, ConfigDigest: cp.ConfigDigest,
		Representations: reps, Aligned: align.Aligned,
		Fragments: records, Quarantined: cp.Quarantine, SwitchPoints: points,
		AudioGroups: audioGroups, CompatibleVideo: video.Compatible, IncompatibleVideo: video.Incompatible,
		Issues: tr.Issues,
		Stats: manifest.Stats{
			Fragments: len(plan.Fragments), Unique: len(tr.Fragments), Quarantined: len(cp.Quarantine),
			RoundTripOK: rtOKN, RoundTripTotal: rtTotal,
			KeyframeHits: kfHits, KeyframeTotal: len(records),
			Representations: len(reps), Aligned: align.Aligned, NotSwitchable: align.NotSwitchable,
			Gaps: gaps, OutOfOrder: ooo, Duplicates: len(tr.Duplicates) + dupSeq,
			Periods: tr.Periods,
		},
	}
	hash := manifest.ResultHash(records, points, align.Aligned)
	if mf.Fragments == nil {
		mf.Fragments = []manifest.FragmentRecord{}
	}
	if mf.Quarantined == nil {
		mf.Quarantined = []manifest.QuarantineRecord{}
	}
	if mf.SwitchPoints == nil {
		mf.SwitchPoints = []manifest.SwitchPoint{}
	}
	if mf.AudioGroups == nil {
		mf.AudioGroups = []metadata.AudioGroup{}
	}
	if mf.CompatibleVideo == nil {
		mf.CompatibleVideo = []string{}
	}
	if mf.IncompatibleVideo == nil {
		mf.IncompatibleVideo = []string{}
	}
	if mf.Issues == nil {
		mf.Issues = []timeline.Issue{}
	}
	return mf, hash, align
}
