package pipeline

import (
	"io"
	"os"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"mediad/internal/capture"
	"mediad/internal/clock"
	"mediad/internal/fault"
	"mediad/internal/reorder"
)

func scanAndReorder(cfg Config, res *Result) (*CaptureReport, []reorder.Block, error) {
	f, err := os.Open(cfg.InputPath)
	if err != nil {
		return nil, nil, fault.Usage("scan", err)
	}
	defer f.Close()
	inputInfo, err := f.Stat()
	if err != nil {
		return nil, nil, fault.Usage("scan", err)
	}
	sc, err := capture.NewScanner(f, inputInfo.Size())
	if err != nil {
		return nil, nil, err
	}
	clk, err := clock.New(cfg.Rate)
	if err != nil {
		return nil, nil, fault.Usage("clock", err)
	}
	buf := reorder.NewBuffer(clk, cfg.Channels, cfg.PacketFrames)

	for {
		rec, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		res.Records++
		if rec.Kind == capture.KindRTCP {
			if packets, err := rtcp.Unmarshal(rec.Packet); err == nil {
				for _, p := range packets {
					if sr, ok := p.(*rtcp.SenderReport); ok {
						clk.ApplySenderReport(sr, rec.ArrivalNS)
					}
				}
			}
			continue
		}
		if !rec.CRCValid {
			continue
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(rec.Packet); err != nil {
			sc.Report.Add(fault.Damage{Offset: rec.Offset, Kind: "bad_rtp", Message: err.Error(), Recoverable: true})
			continue
		}
		if err := buf.Push(&pkt, rec.ArrivalNS); err != nil {
			return nil, nil, fault.Unrecoverable("reorder", err)
		}
	}

	blocks, stats, err := buf.Flush()
	if err != nil {
		return nil, nil, fault.Unrecoverable("reorder", err)
	}
	pos := int64(0)
	for _, b := range blocks {
		next, err := clk.AdvanceOutput(pos, int64(b.Samples))
		if err != nil {
			return nil, nil, fault.Unrecoverable("clock", err)
		}
		pos = next
	}
	res.Reorder = stats

	damageOffset := int64(-1)
	for _, d := range sc.Report.Damages {
		if d.Kind == "single_bit_crc_damage" {
			damageOffset = d.Offset
			break
		}
		if d.Kind == "bad_crc" && damageOffset < 0 {
			damageOffset = d.Offset
		}
	}
	errBytes := int64(-1)
	if damageOffset >= 0 && cfg.ExpectedDamageOffset >= 0 {
		errBytes = damageOffset - cfg.ExpectedDamageOffset
		if errBytes < 0 {
			errBytes = -errBytes
		}
	}
	res.DamageOffset = damageOffset
	res.DamageOffsetError = errBytes

	report := &CaptureReport{
		Records: res.Records, IndexRebuilt: res.IndexRebuilt, Damages: sc.Report.Damages,
		DamageOffset: damageOffset, DamageOffsetError: errBytes, Stats: stats,
		PacketFrames: cfg.PacketFrames, Rate: cfg.Rate, Channels: cfg.Channels,
	}
	return report, blocks, nil
}
