// Package pipeline wires the media kernel together: manifest discovery,
// container adapters, RTP recovery, timeline alignment and lossless remux.
// It owns the per-run report and the exit-status classification.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pion/rtp"

	"mediad/internal/adapter"
	"mediad/internal/fixture"
	"mediad/internal/manifest"
	"mediad/internal/media"
	"mediad/internal/recovery"
	"mediad/internal/remux"
	"mediad/internal/timeline"
)

// Mode selects how segments become samples.
type Mode string

const (
	ModeSegments Mode = "segments" // manifest + container adapters
	ModeRTP      Mode = "rtp"      // RTP trace through recovery
)

// Config drives one run.
type Config struct {
	Root      string
	OutDir    string
	Mode      Mode
	Manifest  string // explicit manifest path; empty auto-detects
	Container adapter.Container
	Limits    media.Limits
	Align     timeline.AlignOptions
	Remux     remux.Options
	// RequiredKeys rejects output when any is absent.
	RequiredKeys []media.TrackKey
}

// Report is the machine-checkable result printed by the CLI.
type Report struct {
	Profile          string               `json:"profile"`
	Seed             int64                `json:"seed,omitempty"`
	OutputSamples    int                  `json:"output_samples"`
	OutputTracks     int                  `json:"output_tracks"`
	SamplesPerTrack  map[string]int       `json:"samples_per_track,omitempty"`
	OutputSHA256     string               `json:"output_sha256,omitempty"`
	SamplesSHA256    string               `json:"samples_sha256,omitempty"`
	HashMatch        bool                 `json:"hash_match"`
	SyncP95MS        float64              `json:"sync_p95_ms"`
	SyncMaxMS        float64              `json:"sync_max_ms"`
	SegmentP95MS     float64              `json:"segment_p95_ms"`
	SegmentP99MS     float64              `json:"segment_p99_ms"`
	Segments         int                  `json:"segments"`
	RejectedSegments int                  `json:"rejected_segments"`
	LostPackets      int                  `json:"lost_packets"`
	Unrecoverable    int                  `json:"unrecoverable_packets"`
	DuplicatePackets int                  `json:"duplicate_packets"`
	RecoveredPackets int                  `json:"recovered_packets"`
	LossRecoveryPct  float64              `json:"loss_recovery_pct"`
	LatencyP95MS     float64              `json:"latency_p95_ms"`
	Fits             []timeline.CameraFit `json:"fits,omitempty"`
	ErrCode          string               `json:"err_code,omitempty"`
	Errors           []string             `json:"errors,omitempty"`
	ElapsedMS        int64                `json:"elapsed_ms"`
}

// Run executes the pipeline and returns a report. A non-nil error carries a
// classification; the caller maps it to an exit status.
func Run(cfg Config) (*Report, error) {
	start := time.Now()
	if cfg.Limits == (media.Limits{}) {
		cfg.Limits = media.DefaultLimits()
	}
	if cfg.OutDir == "" {
		cfg.OutDir = filepath.Join(cfg.Root, "out")
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, media.Wrap(media.CodeIO, "pipeline-outdir", media.SourcePos{}, err, "")
	}
	rep := &Report{SamplesPerTrack: make(map[string]int)}
	var err error
	switch cfg.Mode {
	case ModeRTP:
		err = runRTP(cfg, rep)
	default:
		err = runSegments(cfg, rep)
	}
	if err != nil {
		rep.ErrCode = string(media.CodeOf(err))
		e, _ := media.AsError(err)
		rep.Errors = append(rep.Errors, "")
		if e != nil {
			rep.Errors[len(rep.Errors)-1] = e.Error()
		} else {
			rep.Errors[len(rep.Errors)-1] = err.Error()
		}
		rep.ElapsedMS = time.Since(start).Milliseconds()
		return rep, err
	}
	rep.ElapsedMS = time.Since(start).Milliseconds()
	return rep, nil
}

func resolveManifest(cfg Config) (string, string, error) {
	if cfg.Manifest != "" {
		if strings.EqualFold(filepath.Ext(cfg.Manifest), ".mpd") {
			return cfg.Manifest, "dash", nil
		}
		return cfg.Manifest, "hls", nil
	}
	root := cfg.Root
	for _, cand := range []string{
		filepath.Join(root, "media", "master.m3u8"),
		filepath.Join(root, "media", "stream.mpd"),
	} {
		if _, err := os.Stat(cand); err == nil {
			if filepath.Ext(cand) == ".mpd" {
				return cand, "dash", nil
			}
			return cand, "hls", nil
		}
	}
	return "", "", media.New(media.CodeMissingEntry, "manifest", media.SourcePos{File: root},
		"no master.m3u8 or stream.mpd under %s", filepath.Join(root, "media"))
}

// runSegments is the CMAF/TS path.
func runSegments(cfg Config, rep *Report) error {
	path, format, err := resolveManifest(cfg)
	if err != nil {
		return err
	}
	var man *manifest.Manifest
	if format == "dash" {
		man, err = manifest.ReadDASH(path, manifest.Options{Root: cfg.Root, Limit: cfg.Limits})
	} else {
		man, err = manifest.ReadHLS(path, manifest.Options{Root: cfg.Root, Limit: cfg.Limits})
	}
	if err != nil {
		return err
	}
	kind := cfg.Container
	if kind == "" {
		kind = adapter.ContainerCMAF
	}
	ad, err := adapter.DefaultRegistry().Get(kind, 1)
	if err != nil {
		return err
	}

	byRend := make(map[string][]manifest.Segment)
	var rendOrder []string
	for _, seg := range man.Segments {
		cam := "cam"
		if seg.Rendition != nil {
			cam = seg.Rendition.Camera
		}
		if _, ok := byRend[cam]; !ok {
			rendOrder = append(rendOrder, cam)
		}
		byRend[cam] = append(byRend[cam], seg)
	}
	sort.Strings(rendOrder)

	rep.Segments = len(man.Segments)
	keySet := adapter.NewTrackSet()
	var trackInputs []*remux.TrackInput
	var durations []time.Duration
	var firstReject error

	for _, cam := range rendOrder {
		segs := byRend[cam]
		rend := segs[0].Rendition
		initPath := filepath.Join(filepath.Dir(filepath.Join(cfg.Root, filepath.FromSlash(segs[0].RelPath))), "init.mp4")
		initData, err := os.ReadFile(initPath)
		if err != nil {
			return media.Wrap(media.CodeIO, "read-init", segs[0].Pos, err, "camera %s", cam)
		}
		initSeg, err := ad.ParseInit(initData, segs[0].Pos)
		if err != nil {
			return err
		}
		desc := adapter.RenditionDesc{Camera: cam}
		if rend != nil {
			desc.Language, desc.Role, desc.Codec, desc.Type = rend.Language, rend.Role, rend.Codec, rend.Type
		}
		keys, err := adapter.MapTracks(initSeg, desc)
		if err != nil {
			return err
		}
		if err := adapter.CheckRequired(keys, cfg.RequiredKeys, segs[0].Pos); err != nil {
			return err
		}
		if err := keySet.Add(cam, keys); err != nil {
			return err
		}

		refs := make(map[uint32]int64)
		var samples []media.Sample
		for _, seg := range segs {
			full := filepath.Join(cfg.Root, filepath.FromSlash(seg.RelPath))
			data, err := os.ReadFile(full)
			if err != nil {
				return media.Wrap(media.CodeIO, "read-segment", seg.Pos, err, "%s", seg.RelPath)
			}
			t0 := time.Now()
			got, err := ad.ParseSegment(adapter.SegmentRequest{
				Data: data, Pos: seg.Pos, SegmentIndex: seg.Seq, URI: seg.RelPath,
				Init: initSeg, Keys: keys, Refs: refs, Limits: cfg.Limits,
			})
			durations = append(durations, time.Since(t0))
			if err != nil {
				// Damaged segment: isolate it and record why. It is never
				// silently skipped and never included in output.
				if !media.Isolated(err) {
					return err
				}
				rep.RejectedSegments++
				if firstReject == nil {
					firstReject = err
				}
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %s", seg.RelPath, errCodeOf(err)))
				continue
			}
			for _, s := range got {
				refs[trackIDForKey(initSeg, s.Key)] = s.DTS
			}
			samples = append(samples, got...)
		}
		if len(samples) == 0 {
			continue
		}
		for id, key := range keys {
			var subset []media.Sample
			for _, s := range samples {
				if s.Key == key {
					subset = append(subset, s)
				}
			}
			if len(subset) == 0 {
				continue
			}
			info := initSeg.TrackByID(id)
			trackInputs = append(trackInputs, &remux.TrackInput{
				Key: key, TrackID: id, Kind: info.Kind, Timescale: info.Timescale,
				SampleEntry: info.SampleEntry, Width: info.Width, Height: info.Height, Samples: subset,
			})
		}
	}

	rep.SegmentP95MS = durationPct(durations, 95)
	rep.SegmentP99MS = durationPct(durations, 99)
	rep.LatencyP95MS = rep.SegmentP95MS

	// A truncated/damaged segment means the run must not present output as
	// successful: report it, write nothing, and exit with the isolated status.
	if firstReject != nil {
		return firstReject
	}
	if len(trackInputs) == 0 {
		return media.New(media.CodeNoTrack, "pipeline", media.SourcePos{}, "no usable tracks")
	}
	return finalize(cfg, rep, trackInputs, true)
}

// finalize optionally aligns tracks, normalizes the timeline to a zero origin,
// and writes the verified output.
func finalize(cfg Config, rep *Report, tracks []*remux.TrackInput, align bool) error {
	if align && len(tracks) > 1 {
		ts := make([]*timeline.TrackSamples, len(tracks))
		for i, t := range tracks {
			ts[i] = &timeline.TrackSamples{Key: t.Key, Timescale: t.Timescale, Samples: t.Samples}
		}
		res, err := timeline.Align(ts, cfg.Align)
		if err != nil {
			return err
		}
		rep.Fits = res.Fits
		rep.SyncP95MS = res.P95SyncMS
		rep.SyncMaxMS = res.MaxSyncMS
		for i, t := range tracks {
			out := *t
			out.Samples = res.Tracks[i].Samples
			tracks[i] = &out
		}
	}
	normalizeOrigin(tracks)
	sum, _, err := remux.Write(cfg.OutDir, tracks, cfg.Remux)
	if err != nil {
		return err
	}
	rep.OutputSamples = sum.OutputSamples
	rep.OutputTracks = sum.OutputTracks
	rep.SamplesPerTrack = sum.SamplesPerTrack
	rep.OutputSHA256 = sum.OutputSHA256
	rep.SamplesSHA256 = sum.SampleSHA256
	rep.HashMatch = sum.SourceHashMatch
	return nil
}

// normalizeOrigin shifts every sample so the global minimum DTS is zero,
// preserving relative timing, durations and VFR while keeping tfdt encodable.
func normalizeOrigin(tracks []*remux.TrackInput) {
	var min int64
	first := true
	for _, t := range tracks {
		for _, s := range t.Samples {
			if first || s.DTS < min {
				min = s.DTS
				first = false
			}
		}
	}
	if first || min == 0 {
		return
	}
	for _, t := range tracks {
		for i := range t.Samples {
			t.Samples[i].DTS -= min
			t.Samples[i].PTS -= min
		}
	}
}

// runRTP is the recovery path over fixture RTP traces.
func runRTP(cfg Config, rep *Report) error {
	rtpDir := filepath.Join(cfg.Root, "media", "rtp")
	entries, err := os.ReadDir(rtpDir)
	if err != nil {
		return media.Wrap(media.CodeIO, "rtp-dir", media.SourcePos{File: rtpDir}, err, "")
	}
	ad, err := adapter.DefaultRegistry().Get(adapter.ContainerRTP, 1)
	if err != nil {
		return err
	}
	var trackInputs []*remux.TrackInput
	totalLost, totalUnrecoverable, totalDup, totalRecovered := 0, 0, 0, 0
	var latencies []time.Duration

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		tracePath := filepath.Join(rtpDir, entry.Name())
		trace, err := fixture.ReadRTPTrace(tracePath, cfg.Limits)
		if err != nil {
			return err
		}
		t0 := time.Now()
		packets := make([]*rtp.Packet, 0, len(trace.Received))
		repairs := make(recovery.Redundancy, len(trace.Received))
		for _, rec := range trace.Received {
			packets = append(packets, trace.ToPacket(rec))
			for _, r := range rec.Repairs {
				repairs[rec.Seq] = append(repairs[rec.Seq], recovery.RepairCopy{
					Orig: r.Orig, Payload: r.Payload, Timestamp: r.Timestamp, Marker: r.Marker,
				})
			}
		}
		res, err := recovery.Recover(trace.SSRC, trace.Camera, packets, repairs,
			recovery.Options{Limits: cfg.Limits, SentSeqs: trace.SentSeqs})
		if err != nil {
			return err
		}
		totalLost += res.LostInTransit
		totalUnrecoverable += res.Unrecoverable
		totalDup += res.DupCount
		totalRecovered += res.RecoveredCount

		key := media.TrackKey{Camera: trace.Camera, Language: "und", Codec: "avc1", Role: "main"}
		init := &adapter.InitSegment{Tracks: []*adapter.InitTrack{{
			ID: 1, Kind: media.KindVideo, Codec: "avc1", EntryType: "avc1", Timescale: media.TickRate,
		}}}
		keys := map[uint32]media.TrackKey{1: key}
		samples, err := ad.ParseSegment(adapter.SegmentRequest{
			Pos: media.SourcePos{File: tracePath}, Init: init, Keys: keys, Limits: cfg.Limits,
			// res.Packets already interleaves received and repair-restored packets.
			RTP: &adapter.RTPInput{Packets: resPackets(res)},
		})
		latencies = append(latencies, time.Since(t0))
		if err != nil {
			return err
		}
		trackInputs = append(trackInputs, &remux.TrackInput{
			Key: key, TrackID: 1, Kind: media.KindVideo, Timescale: media.TickRate,
			SampleEntry: rtpSampleEntry(), Width: 1920, Height: 1080, Samples: samples,
		})
	}
	if len(trackInputs) == 0 {
		return media.New(media.CodeNoTrack, "rtp", media.SourcePos{}, "no RTP traces found")
	}
	rep.LostPackets = totalLost
	rep.Unrecoverable = totalUnrecoverable
	rep.DuplicatePackets = totalDup
	rep.RecoveredPackets = totalRecovered
	rep.LatencyP95MS = durationPct(latencies, 95)
	rep.SegmentP95MS = rep.LatencyP95MS
	// Recovery rate is the fraction of otherwise-missing packets that were
	// restored from redundancy: recovered / (recovered + unrecoverable).
	if denom := totalRecovered + totalUnrecoverable; denom > 0 {
		rep.LossRecoveryPct = 100 * float64(totalRecovered) / float64(denom)
	} else {
		rep.LossRecoveryPct = 100
	}
	rep.Segments = len(trackInputs)
	return finalize(cfg, rep, trackInputs, false)
}

func resPackets(res *recovery.Result) []*rtp.Packet {
	out := make([]*rtp.Packet, 0, len(res.Packets))
	for _, p := range res.Packets {
		pkt := &rtp.Packet{Header: rtp.Header{
			Version: 2, Marker: p.Marker, PayloadType: p.PT,
			SequenceNumber: p.Wire, Timestamp: uint32(p.Timestamp), SSRC: p.SSRC,
		}}
		pkt.Payload = p.Payload
		out = append(out, pkt)
	}
	return out
}

func rtpSampleEntry() []byte {
	entry := make([]byte, 86)
	entry[3] = 86
	copy(entry[4:], "avc1")
	return entry
}

func durationPct(d []time.Duration, p float64) float64 {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(float64(len(s))*p/100+0.5) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return float64(s[idx].Microseconds()) / 1000
}

func trackIDForKey(init *adapter.InitSegment, key media.TrackKey) uint32 {
	for _, t := range init.Tracks {
		if adapter.NormalizeCodec(t.Codec) == key.Codec {
			return t.ID
		}
	}
	if len(init.Tracks) > 0 {
		return init.Tracks[0].ID
	}
	return 1
}

func errCodeOf(err error) string { return string(media.CodeOf(err)) }
