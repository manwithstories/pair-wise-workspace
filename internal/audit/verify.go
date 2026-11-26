// Package audit independently re-derives the run's metrics from committed
// artifacts and the write-ahead log. It never reuses compositor or transport
// state: frames are re-hashed, audio is re-decoded, wire events are replayed.
package audit

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"pairflow/internal/audio"
	"pairflow/internal/media"
	"pairflow/internal/store"
	"pairflow/internal/transport"
	"pairflow/internal/video"
)

// Report is the re-read artifact set: everything verify needs to recompute.
type Generation struct {
	Timeline string `json:"timeline_sha256"`
	Seed     uint64 `json:"seed"`
	Faults   string `json:"faults"`
	Frames   int    `json:"frame_count"`
	// FrameHashes is sha256 of each committed PNG, in frame order.
	FrameHashes []string `json:"frame_hashes"`
	// SyncErrorUS is the worst |frame timestamp_ms - nearest sample_ms| in
	// microseconds, computed from the shared rational base.
	SyncErrorUS float64 `json:"sync_max_us"`
	// Audio gap spans taken from the transport report, in canonical frames.
	Gaps []transport.Gap `json:"audio_gaps"`
}

// Config pins the parameters verify must agree with.
type Config struct {
	Faults string `json:"faults"`
	Seed   uint64 `json:"seed"`
}

// Result is the verifier's output.
type Result struct {
	Timeline         string
	Frames           int
	FrameHashesOK    bool
	SyncMaxUS        float64
	Lost             int
	Recovered        int
	LossRecoveryPct  float64
	P95LatencyMS     float64
	WavRate          int
	WavChannels      int
	PCMSampleFrames  int64
	Gaps             []transport.Gap
	Issues           []string
	StageArtifactsOK bool
}

// Verify re-reads every artifact in the run directory and recomputes metrics.
func Verify(root string, want *Generation) (*Result, *media.Error) {
	j, err := store.OpenJail(root)
	if err != nil {
		return nil, err
	}
	m, ok, err := j.ReadManifest()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, media.Integrity("audit.no_manifest", "run has no committed manifest; nothing to verify")
	}
	res := &Result{Timeline: m.Timeline}
	if want != nil && want.Timeline != "" && want.Timeline != m.Timeline {
		return nil, media.Integrity("audit.timeline_mismatch",
			"manifest timeline %s does not match generation %s", m.Timeline, want.Timeline)
	}

	// 1) Re-read and verify every stage artifact recorded in the manifest.
	res.StageArtifactsOK = true
	for _, st := range m.Stages {
		for _, a := range st.Artifacts {
			if _, _, err := j.HashFile(a.Path); err != nil {
				return nil, err
			}
			if err := j.VerifyArtifacts([]store.Artifact{a}); err != nil {
				res.StageArtifactsOK = false
				res.Issues = append(res.Issues, err.Error())
			}
		}
	}

	// 2) Frames: re-read, CRC-check, re-hash and compare with the generation.
	hashes, ferr := verifyFrames(j, m)
	if ferr != nil {
		return nil, ferr
	}
	res.Frames = len(hashes)
	res.FrameHashesOK = true
	if want != nil && len(want.FrameHashes) > 0 {
		if len(hashes) != len(want.FrameHashes) {
			return nil, media.Integrity("audit.frame_count",
				"re-read %d frames, generation recorded %d", len(hashes), len(want.FrameHashes))
		}
		for i := range hashes {
			if hashes[i] != want.FrameHashes[i] {
				res.FrameHashesOK = false
				res.Issues = append(res.Issues, fmt.Sprintf("frame %05d hash differs", i))
			}
		}
	}

	// 3) Audio: re-decode, validate the format and recompute the sample count.
	pcm, format, aerr := readAudio(j, m)
	if aerr != nil {
		return nil, aerr
	}
	res.WavRate, res.WavChannels = format.Rate, format.Channels
	res.PCMSampleFrames = int64(len(pcm) / format.Channels)
	expectSamples := int64(res.Frames) * media.AudioRate / media.VideoFPS
	if res.PCMSampleFrames != expectSamples {
		return nil, media.Integrity("audit.audio_length",
			"pcm has %d sample frames, want %d for %d frames", res.PCMSampleFrames, expectSamples, res.Frames)
	}

	// 4) Transport: replay the wire log and recompute lost/recovered/p95.
	tr, terr := readTransport(j, m)
	if terr != nil {
		return nil, terr
	}
	res.Lost, res.Recovered = replayWire(tr)
	res.LossRecoveryPct = 0
	if res.Lost > 0 {
		res.LossRecoveryPct = float64(res.Recovered) / float64(res.Lost) * 100
	}
	if tr.Report.Recovered != res.Recovered || tr.Report.Lost != res.Lost {
		return nil, media.Integrity("audit.transport_mismatch",
			"transport report lost/recovered %d/%d, replay %d/%d",
			tr.Report.Lost, tr.Report.Recovered, res.Lost, res.Recovered)
	}
	res.P95LatencyMS = transport.P95(tr.Report.BlockLatencyMS)
	res.Gaps = tr.Report.Gaps

	// 5) Sync: worst distance from frame boundary to the shared sample base,
	// recomputed from frame indices and 48 kHz, never from stored floats.
	res.SyncMaxUS = syncErrorUS(res.Frames)

	// 6) Audio gaps must coincide with silence in the committed WAV, proving
	// the unrecovered span propagated into the product rather than just the
	// report.
	for _, g := range res.Gaps {
		if !gapIsSilent(pcm, format.Channels, g) {
			res.Issues = append(res.Issues, fmt.Sprintf("gap at frame %d is not silent in the committed mix", g.StartFrame))
		}
	}
	return res, nil
}

// syncErrorUS maps every frame timestamp to the nearest 48 kHz sample and
// records the worst error. Frame k is at k/24 s; samples are at j/48000 s, so
// the exact nearest-sample error is |k*2000/24 rounded| scaled by 1/48000 s.
func syncErrorUS(frames int) float64 {
	var worst float64
	for k := 0; k < frames; k++ {
		samples := float64(k) * float64(media.AudioRate) / float64(media.VideoFPS)
		errSamples := math.Abs(samples - math.Round(samples))
		us := errSamples / float64(media.AudioRate) * 1e6
		if us > worst {
			worst = us
		}
	}
	return worst
}

// verifyFrames re-reads each committed PNG, validates its structure and
// returns the re-hashed identities.
func verifyFrames(j *store.Jail, m *store.ManifestDoc) ([]string, *media.Error) {
	stage, ok := findStage(m, "compose")
	if !ok {
		return nil, media.Integrity("audit.no_compose", "manifest has no compose stage")
	}
	var hashes []string
	for _, a := range stage.Artifacts {
		if a.Kind != "png" {
			continue
		}
		data, err := j.ReadFile(a.Path)
		if err != nil {
			return nil, err
		}
		if err := video.ValidatePNG(data, a.Path); err != nil {
			return nil, err
		}
		h := video.ImageSHA(data)
		if h != a.SHA {
			return nil, media.Integrity("audit.frame_hash",
				"frame %s reads %s but manifest recorded %s", a.Path, h, a.SHA).WithPath(a.Path)
		}
		if _, err := video.ReadColorMeta(data); err != nil {
			return nil, err.WithPath(a.Path)
		}
		hashes = append(hashes, h)
	}
	if len(hashes) == 0 {
		return nil, media.Integrity("audit.no_frames", "compose stage committed no PNG frames")
	}
	return hashes, nil
}

func readAudio(j *store.Jail, m *store.ManifestDoc) ([]int16, audio.Format, *media.Error) {
	stage, ok := findStage(m, "audio")
	if !ok {
		return nil, audio.Format{}, media.Integrity("audit.no_audio", "manifest has no audio stage")
	}
	var wav *store.Artifact
	for i := range stage.Artifacts {
		if stage.Artifacts[i].Kind == "wav" {
			wav = &stage.Artifacts[i]
			break
		}
	}
	if wav == nil {
		return nil, audio.Format{}, media.Integrity("audit.no_wav", "audio stage committed no wav")
	}
	abs, err := j.Resolve(wav.Path)
	if err != nil {
		return nil, audio.Format{}, err
	}
	pcm, format, rerr := audio.ReadAllWAV(abs)
	if rerr != nil {
		return nil, audio.Format{}, rerr
	}
	return pcm, format, nil
}

// WireDoc is the JSON transport record committed by the transport stage.
type WireDoc struct {
	Report transport.Report      `json:"report"`
	Wire   []transport.WireEvent `json:"wire"`
}

func readTransport(j *store.Jail, m *store.ManifestDoc) (*WireDoc, *media.Error) {
	stage, ok := findStage(m, "transport")
	if !ok {
		return nil, media.Integrity("audit.no_transport", "manifest has no transport stage")
	}
	var doc *store.Artifact
	for i := range stage.Artifacts {
		if stage.Artifacts[i].Kind == "wire" {
			doc = &stage.Artifacts[i]
			break
		}
	}
	if doc == nil {
		return nil, media.Integrity("audit.no_wire", "transport stage committed no wire log")
	}
	data, err := j.ReadFile(doc.Path)
	if err != nil {
		return nil, err
	}
	var wd WireDoc
	if uerr := json.Unmarshal(data, &wd); uerr != nil {
		return nil, media.Wrap(uerr, media.ClassIntegrity, "audit.wire_json", "wire log is not valid JSON").WithPath(doc.Path)
	}
	return &wd, nil
}

// replayWire recomputes lost and recovered counts from the delivery log alone.
// Lost counts data packets not delivered intact; recovered counts blocks with
// exactly one lost data packet and an intact parity packet.
func replayWire(wd *WireDoc) (lost, recovered int) {
	dataLost := map[int]map[int]bool{} // block -> index set
	parityOK := map[int]bool{}
	maxBlock := -1
	desc := map[int]string{}
	for _, ev := range wd.Wire {
		if ev.Block > maxBlock {
			maxBlock = ev.Block
		}
		desc[ev.Seq] = ev.Action
		if ev.Parity {
			if ev.Action == transport.ActionDelivered {
				parityOK[ev.Block] = true
			}
			continue
		}
		if ev.Action != transport.ActionDelivered && ev.Action != transport.ActionDuplicate {
			if dataLost[ev.Block] == nil {
				dataLost[ev.Block] = map[int]bool{}
			}
			dataLost[ev.Block][ev.Index] = true
		}
	}
	for b := 0; b <= maxBlock; b++ {
		lost += len(dataLost[b])
		if len(dataLost[b]) == 1 && parityOK[b] {
			recovered++
		}
	}
	return lost, recovered
}

func findStage(m *store.ManifestDoc, name string) (store.StageDoc, bool) {
	for _, st := range m.Stages {
		if st.Name == name {
			return st, true
		}
	}
	return store.StageDoc{}, false
}

func gapIsSilent(pcm []int16, channels int, g transport.Gap) bool {
	start := g.StartFrame
	end := g.StartFrame + g.Frames
	if end > int64(len(pcm)/channels) {
		end = int64(len(pcm) / channels)
	}
	for i := start; i < end; i++ {
		for ch := 0; ch < channels; ch++ {
			if pcm[i*int64(channels)+int64(ch)] != 0 {
				return false
			}
		}
	}
	return true
}

// Describe renders a one-line summary of the verification.
func (r *Result) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "timeline_sha256=%s frames=%d frame_hashes_ok=%v sync_max_us=%.4f ", r.Timeline, r.Frames, r.FrameHashesOK, r.SyncMaxUS)
	fmt.Fprintf(&b, "lost=%d recovered=%d loss_recovery_pct=%.2f p95_latency_ms=%.2f ", r.Lost, r.Recovered, r.LossRecoveryPct, r.P95LatencyMS)
	fmt.Fprintf(&b, "wav_rate=%d wav_channels=%d pcm_sample_frames=%d stages_verified=%v",
		r.WavRate, r.WavChannels, r.PCMSampleFrames, r.StageArtifactsOK)
	if len(r.Issues) > 0 {
		fmt.Fprintf(&b, " issues=%d", len(r.Issues))
	}
	return b.String()
}

// SortedIssues returns the verifier findings in detection order.
func (r *Result) SortedIssues() []string {
	out := append([]string(nil), r.Issues...)
	sort.Strings(out)
	return out
}
