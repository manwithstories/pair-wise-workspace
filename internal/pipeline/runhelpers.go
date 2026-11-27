package pipeline

import (
	"tsqc/internal/clock"
	"tsqc/internal/ts"
)

func demuxStream(buf []byte) (*ts.Demux, []ts.AccessUnit, error) {
	pkts, trunc, splitErr := ts.SplitPackets(buf)
	if splitErr != nil && !clock.IsRecoverable(splitErr) {
		return nil, nil, splitErr
	}
	_ = trunc
	d := ts.NewDemux()
	var aus []ts.AccessUnit
	var firstErr error
	for _, raw := range pkts {
		got, err := d.PushPacket(raw)
		aus = append(aus, got...)
		if err != nil {
			if !clock.IsRecoverable(err) {
				return d, aus, err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return d, aus, firstErr
}

func collectGaps(aus []ts.AccessUnit) []GapInterval {
	var out []GapInterval
	var open *GapInterval
	for i := range aus {
		au := &aus[i]
		if au.Unrecoverable {
			if open == nil {
				open = &GapInterval{StartPTS: au.PTS, EndPTS: au.PTS, PID: au.PID, Reason: au.Reason}
			}
			if au.PTS > open.EndPTS {
				open.EndPTS = au.PTS
			}
			continue
		}
		if open != nil && au.Kind == ts.KindVideo && au.Keyframe && au.PTS >= open.EndPTS {
			open.ResumePTS = au.PTS
			open.ResumeIDR = true
			out = append(out, *open)
			open = nil
		}
	}
	if open != nil {
		open.ResumePTS = open.EndPTS
		open.Reason += "+eos"
		out = append(out, *open)
	}
	return out
}
