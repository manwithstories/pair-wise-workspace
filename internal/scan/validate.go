package scan

import (
	"encoding/json"
	"fmt"
	"sort"

	"aircheck/internal/clock"
	"aircheck/internal/fault"
)

// truncTolerance is how far the last packet may fall short of the
// declared duration before the media is called truncated.
const truncTolerance = clock.Timescale / 2 // 500 ms

// packetView extracts the fields needed for timeline validation.
type packetView struct {
	streamIdx int
	pts, dts  int64
	hasPTS    bool
}

// readPackets streams packet timestamps of one file through ffprobe.
func readPackets(path string, streams *ffprobeOut) (map[int][]packetView, error) {
	release := acquire()
	defer release()
	out, err := execOutput("ffprobe", "-v", "error",
		"-show_entries", "packet=stream_index,pts,dts",
		"-of", "json", path)
	if err != nil {
		return nil, err
	}
	var dec struct {
		Packets []map[string]any `json:"packets"`
	}
	if err := json.Unmarshal(out, &dec); err != nil {
		return nil, fmt.Errorf("packet probe undecodable: %w", err)
	}
	byStream := map[int][]packetView{}
	for _, p := range dec.Packets {
		idx, _ := p["stream_index"].(float64)
		pv := packetView{streamIdx: int(idx)}
		if v, ok := p["pts"].(float64); ok {
			pv.pts, pv.hasPTS = int64(v), true
		}
		if v, ok := p["dts"].(float64); ok {
			pv.dts = int64(v)
		}
		byStream[pv.streamIdx] = append(byStream[pv.streamIdx], pv)
	}
	return byStream, nil
}

// validateTimeline checks per-track monotonicity (with 33-bit unwrap for
// MPEG-TS) and detects truncation by comparing the last packet tick with
// the declared duration.
func validateTimeline(r *Rendition, path string, fo *ffprobeOut) error {
	pkts, err := readPackets(path, fo)
	if err != nil {
		return err
	}
	isTS := fo.Format.FormatName == "mpegts"
	declared := int64(parseFloat(fo.Format.Duration) * float64(clock.Timescale))
	if err := clock.CheckDuration(declared, r.Name); err != nil {
		return err
	}
	r.DurationTicks = declared
	for i, s := range fo.Streams {
		trackID := r.Tracks[i].ID
		ts, err := parseTimeBase(s.TimeBase)
		if err != nil {
			return fault.New(fault.CodeRuntime, trackID, -1, false, "%v", err)
		}
		list := pkts[s.Index]
		if len(list) == 0 && (s.CodecType == "video" || s.CodecType == "audio") {
			return fault.New(fault.CodeMissingTrack, trackID, -1, false,
				"no packets for %s stream", s.CodecType)
		}
		var prev, maxTick int64
		for k, pv := range list {
			v := pv.pts
			if !pv.hasPTS {
				v = pv.dts
			}
			if isTS {
				v = clock.Unwrap33(prev, v)
			}
			tick, err := clock.FromRational(v, ts, trackID)
			if err != nil {
				return err
			}
			if k > 0 {
				if err := clock.CheckMonotonic(prev, tick, trackID); err != nil {
					return err
				}
			}
			prev, maxTick = tick, tick
		}
		if s.CodecType == "video" && declared-maxTick > truncTolerance {
			return fault.New(fault.CodeTruncated, trackID, maxTick, false,
				"declared %s but last packet at %s",
				clock.String(declared), clock.String(maxTick))
		}
	}
	return nil
}

// ScanPackage probes every rendition of a delivery package and parses
// all timed events. The result is deterministic for fixed inputs.
func ScanPackage(root string, del *Delivery) (*Result, error) {
	res := &Result{
		Package:      del.Package,
		SegmentTicks: int64(del.SegmentMS) * 90,
	}
	for _, rd := range del.Renditions {
		path := joinPath(root, rd.File)
		fo, err := probeFile(path)
		if err != nil {
			return nil, err
		}
		rend := Rendition{Name: rd.Name, File: rd.File, TargetBPS: rd.TargetBPS}
		for _, s := range fo.Streams {
			t, err := toTrack(s, rd.File)
			if err != nil {
				return nil, err
			}
			rend.Tracks = append(rend.Tracks, t)
		}
		if err := validateTimeline(&rend, path, fo); err != nil {
			return nil, err
		}
		res.Renditions = append(res.Renditions, rend)
		chs, err := chaptersOf(fo, rd.File)
		if err != nil {
			return nil, err
		}
		res.Events = append(res.Events, chs...)
		subs, err := subtitlesOf(path, rd.File, rend.DurationTicks)
		if err != nil {
			return nil, err
		}
		res.Events = append(res.Events, subs...)
	}
	side, err := sidecarEvents(root, res.Renditions)
	if err != nil {
		return nil, err
	}
	res.Events = append(res.Events, side...)
	sort.Slice(res.Events, func(i, j int) bool {
		if res.Events[i].Tick != res.Events[j].Tick {
			return res.Events[i].Tick < res.Events[j].Tick
		}
		return res.Events[i].Type < res.Events[j].Type
	})
	return res, nil
}
