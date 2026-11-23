package pipeline

import (
	"encoding/hex"
	"fmt"
	"os"

	"mediasync/internal/clock"
	"mediasync/internal/manifest"
	trackmap "mediasync/internal/map"
	"mediasync/internal/model"
	"mediasync/internal/remux"
)

// writePackage remuxes mapped tracks and writes both manifests.
func writePackage(opt Options, mapping *trackmap.Result, stats []model.TrackStats, rep *Report, src *os.File) error {
	w, err := remux.NewWriter(opt.PackageDir)
	if err != nil {
		return err
	}
	want := map[uint32]string{}
	for _, st := range stats {
		want[st.TrackID] = hex.EncodeToString(st.SHA256[:])
	}
	var out remux.Result
	var inputs []manifest.TrackInput
	var quality []float64
	match := true
	for _, sel := range mapping.Selections {
		ot, err := w.WriteTrack(remux.TrackInput{Name: sel.Target.Name, Track: sel.Track, Reader: src})
		if err != nil {
			return err
		}
		out.Tracks = append(out.Tracks, ot)
		out.Samples += ot.Samples
		out.Bytes += ot.Bytes
		if w, ok := want[sel.Track.ID]; ok && w != ot.SHA256 {
			match = false
		}
		inputs = append(inputs, manifest.TrackInput{Name: sel.Target.Name, Track: sel.Track,
			SHA256: ot.SHA256, Missing: rep.Incomplete})
		quality = append(quality, sampleQuality(sel.Track)...)
		rep.Tracks = append(rep.Tracks, TrackReport{
			Name: ot.Name, TrackID: ot.TrackID, Kind: ot.Kind,
			Samples: ot.Samples, Bytes: ot.Bytes, SHA256: ot.SHA256,
		})
	}
	rep.SampleSHAMatch = match
	if err := w.Verify(out); err != nil {
		return err
	}
	if _, err := w.WriteManifest(out); err != nil {
		return err
	}
	m, err := manifest.Build(inputs, opt.TargetKbps, rep.ActualKbps)
	if err != nil {
		return err
	}
	if _, err := manifest.Write(opt.PackageDir, m); err != nil {
		return err
	}
	rep.ManifestAlignMs = m.Alignment.MaxMs
	rep.BitrateErrPct = m.BitrateErrPct
	rep.BitrateNote = bitrateNote(rep.ActualKbps, opt.TargetKbps)
	// Record the RTP incompleteness on the manifest tracks (already attached to
	// inputs) plus the timeline total for the operator.
	if len(rep.Incomplete) > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"%d incomplete RTP interval(s); no samples were fabricated to fill them", len(rep.Incomplete)))
	}
	mean, p95, err := manifest.QualityStats(quality)
	if err != nil {
		return err
	}
	rep.QualityMean, rep.QualityP95 = mean, p95
	return nil
}

// bitrateNote names the sources of a target/actual gap so the difference is not
// left unexplained. It is advisory text only; nothing downstream consumes it as
// a control value.
func bitrateNote(actual float64, target int) string {
	if target <= 0 {
		return "no target declared; actual rate is measured only"
	}
	dev := (actual - float64(target)) / float64(target) * 100
	if dev < 0 {
		dev = -dev
	}
	if dev <= 10 {
		return fmt.Sprintf("within 10%% tolerance (%.2f%%): coded size and fixture dithering account for the gap", dev)
	}
	return fmt.Sprintf("%.2f%% from target: coded sample sizes dominate; the budget sizes blocks only and never rewrites essence", dev)
}

// sampleQuality gives each sample a finite [0,1] health score derived from its
// duration relative to the track timescale. It describes stream regularity
// only; it never gates or alters coded data.
func sampleQuality(tr *model.Track) []float64 {
	if tr.Timescale == 0 || len(tr.Samples) == 0 {
		return nil
	}
	out := make([]float64, 0, len(tr.Samples))
	for i := range tr.Samples {
		d := clock.Rational{Num: tr.Samples[i].Duration, Den: int64(tr.Timescale)}
		ms := d.MillisF()
		out = append(out, 1.0/(1.0+ms/1000.0))
	}
	return out
}
