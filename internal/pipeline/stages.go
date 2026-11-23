package pipeline

import (
	"math"
	"sort"
	"time"

	pionrtp "github.com/pion/rtp"

	"mediakernel/internal/clock"
	"mediakernel/internal/fixture"
	"mediakernel/internal/manifest"
	"mediakernel/internal/rtp"
	"mediakernel/internal/sync"
)

// parsedMedia 是解析阶段的输出。
type parsedMedia struct {
	Blocks     []fixture.Block `json:"blocks"`
	DurationNs int64           `json:"duration_ns"`
}

// frame 是一个交付帧；缺失分片置零并计数，绝不伪造。
type frame struct {
	Track    uint64 `json:"track"`
	Index    int    `json:"index"`
	AbsNs    int64  `json:"abs_ns"`
	Keyframe bool   `json:"keyframe"`
	Data     []byte `json:"data"`
	Missing  int    `json:"missing"` // 缺失字节数
}

// delivery 是传输+恢复阶段的输出。
type delivery struct {
	Frames         []frame   `json:"frames"`
	Recovered      int       `json:"recovered"`
	Lost           int       `json:"lost"`
	Late           int       `json:"late"`
	Gaps           []rtp.Gap `json:"gaps"`
	DeliveredBytes int64     `json:"delivered_bytes"`
	VideoRefBytes  int64     `json:"video_ref_bytes"`
	VideoSSE       float64   `json:"video_sse"` // 视频平方误差和
}

// doTransport 模拟网络传输：抖动缓冲、乱序重排、突发丢包与重传恢复。
func (r *runner) doTransport(pm *parsedMedia) *delivery {
	pkts := fixture.Packetize(pm.Blocks, r.params)
	sorted := make([]*fixture.Packet, len(pkts))
	copy(sorted, pkts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Arrival < sorted[j].Arrival })

	bufs := map[uint64]*rtp.Buffer{
		1: rtp.NewBuffer(fixture.VideoClockRate, 30*time.Millisecond, 300*time.Millisecond),
		2: rtp.NewBuffer(fixture.AudioClockRate, 30*time.Millisecond, 300*time.Millisecond),
	}
	recs := map[uint64]*rtp.Recovery{
		1: rtp.NewRecovery(fixture.RetransmitCapacity),
		2: rtp.NewRecovery(fixture.RetransmitCapacity),
	}
	received := map[uint64]map[uint16]*pionrtp.Packet{1: {}, 2: {}}
	gapSeen := map[uint64]int{}
	sendIdx := 0

	play := func(track uint64, now clock.Nanos) {
		for _, p := range bufs[track].PopDue(now) {
			received[track][p.SequenceNumber] = p
		}
		g := bufs[track].Gaps
		for ; gapSeen[track] < len(g); gapSeen[track]++ {
			requestRetransmit(recs[track], g[gapSeen[track]], received[track])
		}
	}
	for _, pkt := range sorted {
		for sendIdx < len(pkts) && pkts[sendIdx].Send <= pkt.Arrival {
			recs[pkts[sendIdx].Track].Cache.Add(pkts[sendIdx].RTP)
			sendIdx++
		}
		if pkt.Lost {
			continue // 突发丢包：包不到达
		}
		bufs[pkt.Track].Push(pkt.RTP, pkt.Arrival)
		play(pkt.Track, pkt.Arrival)
	}
	for track := range bufs {
		play(track, clock.Nanos(math.MaxInt64)) // 排空
	}

	del := &delivery{
		Recovered: recs[1].Recovered + recs[2].Recovered,
		Lost:      recs[1].Lost() + recs[2].Lost(),
		Late:      bufs[1].Stats.Late + bufs[2].Stats.Late,
	}
	del.Gaps = append(del.Gaps, recs[1].Gaps...)
	del.Gaps = append(del.Gaps, recs[2].Gaps...)

	chunk := fixture.PayloadChunk
	for _, blk := range pm.Blocks {
		fr := frame{Track: blk.Track, Index: blk.Index, AbsNs: blk.AbsNs, Keyframe: blk.Keyframe}
		frags := (len(blk.Data) + chunk - 1) / chunk
		var data []byte
		for f := 0; f < frags; f++ {
			want := chunk
			if rest := len(blk.Data) - f*chunk; rest < want {
				want = rest
			}
			frag, ok := findFrag(received[blk.Track], blk.Index, f)
			if !ok {
				fr.Missing += want
				frag = make([]byte, want) // 空洞置零
			}
			data = append(data, frag...)
		}
		fr.Data = data[:len(blk.Data)]
		del.Frames = append(del.Frames, fr)
		del.DeliveredBytes += int64(len(blk.Data)) - int64(fr.Missing)
		if blk.Track == 1 {
			del.VideoRefBytes += int64(len(blk.Data))
			for i := range blk.Data {
				d := int64(blk.Data[i]) - int64(fr.Data[i])
				del.VideoSSE += float64(d * d)
			}
		}
	}
	return del
}

// requestRetransmit 优先用重传缓存恢复；失败则合并记录空洞。
func requestRetransmit(rec *rtp.Recovery, seq uint16, received map[uint16]*pionrtp.Packet) {
	if pkt, ok := rec.Cache.Get(seq); ok {
		rec.Recovered++
		received[seq] = pkt
		return
	}
	if n := len(rec.Gaps); n > 0 && rec.Gaps[n-1].To == seq-1 {
		rec.Gaps[n-1].To = seq
		return
	}
	rec.Gaps = append(rec.Gaps, rtp.Gap{From: seq, To: seq, Reason: "retransmit cache miss"})
}

// findFrag 在已收包中查找指定帧分片的媒体数据。
func findFrag(pkts map[uint16]*pionrtp.Packet, frameIdx, frag int) ([]byte, bool) {
	for _, pkt := range pkts {
		pl := pkt.Payload
		if len(pl) < 4 {
			continue
		}
		if int(pl[0])<<8|int(pl[1]) == frameIdx && int(pl[2]) == frag {
			return pl[4:], true
		}
	}
	return nil, false
}

// doManifest 对齐音画并输出清单与量化指标。
func (r *runner) doManifest() (*Result, error) {
	tracks := map[uint64]*sync.TrackTime{}
	trackBytes := map[uint64]int64{}
	for _, fr := range r.del.Frames {
		tt, ok := tracks[fr.Track]
		if !ok {
			tt = &sync.TrackTime{Track: fr.Track, Type: int(fr.Track), Base: clock.Nanos(math.MaxInt64)}
			tracks[fr.Track] = tt
		}
		abs := clock.Nanos(fr.AbsNs)
		if abs < tt.Base {
			tt.Base = abs
		}
		end := abs + clock.Nanos(frameInterval(fr.Track))
		if end > tt.End {
			tt.End = end
		}
		if fr.Keyframe {
			tt.Keys = append(tt.Keys, abs)
		}
		trackBytes[fr.Track] += int64(len(fr.Data)) - int64(fr.Missing)
	}
	var tts []sync.TrackTime
	for _, tt := range tracks {
		tts = append(tts, *tt)
	}
	sort.Slice(tts, func(i, j int) bool { return tts[i].Track < tts[j].Track })
	al, err := (sync.Policy{Tolerance: 20 * clock.Millisecond}).Align(tts)
	if err != nil {
		return nil, err
	}

	durSec := float64(r.pm.DurationNs) / 1e9
	m := manifest.Manifest{
		StartNs: int64(al.Start), EndNs: int64(al.End),
		DurationNs: r.pm.DurationNs, SourceSHA256: r.srcDigest,
	}
	for _, tt := range tts {
		kind, codec := "video", "V_VP8"
		if tt.Track == 2 {
			kind, codec = "audio", "A_OPUS"
		}
		m.Tracks = append(m.Tracks, manifest.Track{
			Number: tt.Track, Type: kind, Codec: codec,
			BaseNs:      int64(al.Bases[tt.Track]),
			BitrateKbps: float64(trackBytes[tt.Track]) * 8 / durSec / 1000,
		})
	}

	actual := float64(r.del.DeliveredBytes) * 8 / durSec / 1000
	psnr := 99.0
	if r.del.VideoSSE > 0 {
		psnr = 10 * math.Log10(255*255*float64(r.del.VideoRefBytes)/r.del.VideoSSE)
	}
	timelineErr := int64(al.End) - r.pm.DurationNs
	if timelineErr < 0 {
		timelineErr = -timelineErr
	}
	return &Result{
		Metrics: Metrics{
			SourceSHA256:    r.srcDigest,
			Recovered:       r.del.Recovered,
			Lost:            r.del.Lost,
			Late:            r.del.Late,
			TargetKbps:      r.opts.BandwidthKbps,
			ActualKbps:      math.Round(actual*100) / 100,
			BitrateErrPct:   math.Round(math.Abs(actual-float64(r.opts.BandwidthKbps))/float64(r.opts.BandwidthKbps)*10000) / 100,
			PSNRdB:          math.Round(psnr*100) / 100,
			ManifestStartMs: int64(al.Start) / 1_000_000,
			TimelineErrMs:   timelineErr / 1_000_000,
		},
		Manifest: m,
	}, nil
}

func frameInterval(track uint64) time.Duration {
	if track == 2 {
		return fixture.AudioInterval
	}
	return fixture.VideoInterval
}
