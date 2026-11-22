package demo

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediacore/internal/audio"
	"mediacore/internal/clock"
	"mediacore/internal/fingerprint"
	"mediacore/internal/fixture"
	"mediacore/internal/manifest"
	"mediacore/internal/media"
	"mediacore/internal/pipeline"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// readPayload loads one segment's mdat payload from disk.
func readPayload(dir, uri, trackID string, segIndex int) ([]byte, error) {
	f, err := os.Open(filepath.Join(dir, uri))
	if err != nil {
		return nil, clock.Wrap(clock.CodeIO, "read-payload", err)
	}
	defer f.Close()
	ms, err := media.ReadSegment(f, trackID, segIndex, true)
	if err != nil {
		return nil, err
	}
	return ms.Payload, nil
}

func payloadToSamples(payload []byte) []int16 {
	out := make([]int16, len(payload)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(payload[2*i:]))
	}
	return out
}

func silenceNode(p Params) pipeline.Node {
	return pipeline.Node{
		Name: "silence", Deps: []string{"media-index"}, Version: Version,
		New: func() any { return &SilenceResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			hls := ctx.Get("parse-hls").(*HlsResult)
			mi := ctx.Get("media-index").(*MediaIndexResult)
			det, err := audio.NewDetector(fixture.AudioSampleRate, audio.Params{
				WindowSamples: silenceWindow,
				Threshold:     silenceThresh,
			})
			if err != nil {
				return nil, err
			}
			res := &SilenceResult{}
			var track *manifest.Track
			for i := range hls.Tracks {
				if hls.Tracks[i].ID == "a1" {
					track = &hls.Tracks[i]
				}
			}
			if track == nil {
				return nil, clock.At(clock.CodeDependencyMissing, "silence", "a1", -1,
					"audio dependency missing")
			}
			for i, seg := range track.Segments {
				idx := mi.Tracks["a1"][i]
				if idx.Gap || idx.Truncated {
					res.Partial = true // missing audio dependency
					if err := det.Skip(fixture.AudioSegBytes / 2); err != nil {
						return nil, err
					}
					continue
				}
				payload, err := readPayload(p.Dir, seg.URI, "a1", seg.Index)
				if err != nil {
					return nil, err
				}
				if err := det.Feed(payloadToSamples(payload)); err != nil {
					return nil, err
				}
			}
			segs, err := det.Finish()
			if err != nil {
				return nil, err
			}
			res.Segments = segs
			if res.Segments == nil {
				res.Segments = []audio.Segment{}
			}
			return res, nil
		},
		Prov: prov("silence", "1/48000", [2]int{0, fixture.AudioSegs - 1}),
	}
}

// manifestTrack aliases manifest.Track to keep selectors short.

func fingerprintNode(p Params) pipeline.Node {
	return pipeline.Node{
		Name: "fingerprint", Deps: []string{"media-index"}, Version: Version,
		New: func() any { return &FingerprintResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			hls := ctx.Get("parse-hls").(*HlsResult)
			mi := ctx.Get("media-index").(*MediaIndexResult)
			var views []fingerprint.TrackView
			for _, trackID := range []string{"v1", "v2"} {
				h, err := fingerprint.NewHasher(fingerprintWin)
				if err != nil {
					return nil, err
				}
				var all []int16
				var tr *manifest.Track
				for i := range hls.Tracks {
					if hls.Tracks[i].ID == trackID {
						tr = &hls.Tracks[i]
					}
				}
				for i, seg := range tr.Segments {
					idx := mi.Tracks[trackID][i]
					if idx.Gap || idx.Truncated {
						continue // window-multiple sizes keep the phase
					}
					payload, err := readPayload(p.Dir, seg.URI, trackID, seg.Index)
					if err != nil {
						return nil, err
					}
					s := payloadToSamples(payload)
					h.Feed(s)
					all = append(all, s...)
				}
				views = append(views, fingerprint.TrackView{
					Name: trackID, Hashes: h.Hashes(), Samples: all, Window: fingerprintWin,
				})
			}
			dups := fingerprint.FindCrossDuplicates(views)
			if dups == nil {
				dups = []fingerprint.Duplicate{}
			}
			return &FingerprintResult{Count: len(dups), Duplicates: dups}, nil
		},
		Prov: prov("fingerprint", "1/90000", [2]int{fixture.CopiedSegment0, fixture.CopiedSegment1}),
	}
}

func bitrateNode() pipeline.Node {
	return pipeline.Node{
		Name: "bitrate", Deps: []string{"media-index"}, Version: Version,
		New: func() any { return &BitrateResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			mi := ctx.Get("media-index").(*MediaIndexResult)
			// Bitrate is computed over effective media duration only:
			// gaps and truncated segments are excluded, never averaged
			// in as low-bitrate samples.
			var totalKbps float64
			samples := 0
			for _, trackID := range []string{"v1", "a1"} {
				var bytes, segs int
				for _, idx := range mi.Tracks[trackID] {
					if idx.Gap || idx.Truncated {
						continue
					}
					for _, s := range idx.Samples {
						bytes += int(s.Size)
					}
					segs++
				}
				if segs == 0 {
					continue
				}
				dur := float64(segs) * fixture.SegSeconds
				totalKbps += float64(bytes) * 8 / dur / 1000
				samples += segs
			}
			target := fixture.VideoTargetKbps + fixture.AudioTargetKbps
			dev := 0.0
			if target > 0 {
				dev = (totalKbps - target) / target * 100
				if dev < 0 {
					dev = -dev
				}
			}
			return &BitrateResult{
				TargetKbps:   target,
				ActualKbps:   totalKbps,
				DeviationPct: dev,
				Samples:      samples,
			}, nil
		},
		Prov: prov("bitrate", "1/90000", [2]int{0, fixture.VideoSegs - 1}),
	}
}

func rtpNode(p Params) pipeline.Node {
	return pipeline.Node{
		Name: "rtp-check", Version: Version,
		New: func() any { return &RtpResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			data, err := os.ReadFile(filepath.Join(p.Dir, "rtp.bin"))
			if err != nil {
				return nil, clock.Wrap(clock.CodeIO, "rtp-check", err)
			}
			var packets []*rtp.Packet
			for len(data) >= 2 {
				n := int(data[0])<<8 | int(data[1])
				if len(data) < 2+n {
					return nil, clock.At(clock.CodeTruncatedSegment, "rtp-check", "", -1,
						"truncated RTP record")
				}
				pkt := &rtp.Packet{}
				if err := pkt.Unmarshal(data[2 : 2+n]); err != nil {
					return nil, clock.Wrap(clock.CodeInputQuality, "rtp-check", err)
				}
				packets = append(packets, pkt)
				data = data[2+n:]
			}
			ticks, err := media.TicksFromRTP(packets, fixture.VideoTimescale)
			if err != nil {
				return nil, err
			}
			mono := true
			for i := 1; i < len(ticks); i++ {
				if ticks[i].Cmp(ticks[i-1]) <= 0 {
					mono = false
				}
			}
			return &RtpResult{Packets: len(packets), Monotonic: mono}, nil
		},
		Prov: prov("rtp-check", "1/90000", [2]int{0, 0}),
	}
}

func reportNode() pipeline.Node {
	return pipeline.Node{
		Name:    "report",
		Deps:    []string{"timeline", "cut", "silence", "fingerprint", "bitrate", "rtp-check"},
		Version: Version,
		New:     func() any { return &ReportResult{} },
		Run: func(ctx *pipeline.Context) (any, error) {
			tl := ctx.Get("timeline").(*TimelineResult)
			sil := ctx.Get("silence").(*SilenceResult)
			fp := ctx.Get("fingerprint").(*FingerprintResult)
			br := ctx.Get("bitrate").(*BitrateResult)
			return &ReportResult{Summary: Summary{
				TargetKbps:          br.TargetKbps,
				ActualKbps:          br.ActualKbps,
				DeviationPct:        br.DeviationPct,
				SilenceSegments:     len(sil.Segments),
				DuplicateSegments:   fp.Count,
				QualitySamples:      br.Samples,
				ManifestAlignmentMs: tl.AlignmentMs,
			}}, nil
		},
		Prov: prov("report", "1/90000", [2]int{0, fixture.VideoSegs - 1}),
	}
}
