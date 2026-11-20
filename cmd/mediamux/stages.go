package main

import (
	"bytes"
	"context"
	"io"
	"time"

	"mediamux/ingest"
	"mediamux/manifest"
	"mediamux/media"
	"mediamux/pipeline"
	"mediamux/testdata"
	"mediamux/transport"
)

func sourceStage(pkts []*transport.Packet) pipeline.Stage {
	return func(ctx context.Context, in <-chan any, out chan<- any) error {
		for _, p := range pkts {
			if !pipeline.Send(ctx, out, p) {
				return ctx.Err()
			}
		}
		return nil
	}
}

func jitterStage(jb *transport.JitterBuffer) pipeline.Stage {
	return func(ctx context.Context, in <-chan any, out chan<- any) error {
		for {
			v, ok := pipeline.Recv(ctx, in)
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				for _, d := range jb.Flush() {
					if !pipeline.Send(ctx, out, d) {
						return ctx.Err()
					}
				}
				return nil
			}
			p := v.(*transport.Packet)
			jb.Push(p, p.Arrival)
			for _, d := range jb.Drain(p.Arrival) {
				if !pipeline.Send(ctx, out, d) {
					return ctx.Err()
				}
			}
		}
	}
}

type subState struct {
	valid     int
	invalid   int
	errOffset int64
}

// subtitleStage 前转 RTP 交付；上游结束后流式解析字幕并发射事件。
func subtitleStage(data []byte, st *subState) pipeline.Stage {
	return func(ctx context.Context, in <-chan any, out chan<- any) error {
		for {
			v, ok := pipeline.Recv(ctx, in)
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				break
			}
			d := v.(transport.Delivery)
			if !pipeline.Send(ctx, out, item{del: &d}) {
				return ctx.Err()
			}
		}
		p := ingest.NewSRTParser(bytes.NewReader(data), media.FrameRate25, testdata.MaxRecord)
		for {
			ev, err := p.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				me := media.As(err)
				if !me.Recoverable {
					return me
				}
				st.invalid++
				if st.errOffset < 0 {
					st.errOffset = me.Offset
				}
				continue
			}
			st.valid++
			if !pipeline.Send(ctx, out, item{sub: ev}) {
				return ctx.Err()
			}
		}
	}
}

type collector struct {
	aligner        *pipeline.Aligner
	deliveredBytes int64
	gaps           []manifest.Gap
	subs           []manifest.SubRange
	lastMedia      media.Ticks
	pendingGap     *manifest.Gap
}

// sink 是末级慢消费者：逐条处理交付与字幕事件，模拟背压。
func (c *collector) sink(slowMs int64) pipeline.Stage {
	return func(ctx context.Context, in <-chan any, out chan<- any) error {
		frameDur := media.FrameRate25.FrameDuration()
		for {
			v, ok := pipeline.Recv(ctx, in)
			if !ok {
				return ctx.Err()
			}
			if slowMs > 0 {
				time.Sleep(time.Duration(slowMs) * time.Millisecond)
			}
			it := v.(item)
			switch {
			case it.del != nil:
				d := it.del
				if d.Gap {
					if c.pendingGap == nil {
						c.pendingGap = &manifest.Gap{Track: "rtp", Start: c.lastMedia + frameDur}
					}
					continue
				}
				mt := d.Pkt.MediaTime(testdata.ClockRate)
				c.aligner.ObserveReference(mt)
				c.deliveredBytes += int64(len(d.Pkt.Payload))
				c.lastMedia = mt
				if c.pendingGap != nil {
					c.pendingGap.End = mt
					c.gaps = append(c.gaps, *c.pendingGap)
					c.pendingGap = nil
				}
			case it.sub != nil:
				c.aligner.ObserveSubtitle(it.sub.Start)
				c.subs = append(c.subs, manifest.SubRange{Start: it.sub.Start, End: it.sub.End})
			}
		}
	}
}
