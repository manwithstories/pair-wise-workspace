package runner

import (
	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
	rtpinspect "mediaarchive/internal/rtp"
)

// inspectHint parses one RTP hint sample. Gaps are recoverable and recorded
// as damage; malformed packets and SSRC changes are hard failures.
func inspectHint(inspectors map[uint32]*rtpinspect.Inspector, raw demux.Sample,
	damages *[]model.Error, opts Options) error {
	in, ok := inspectors[raw.TrackID]
	if !ok {
		in = &rtpinspect.Inspector{}
		inspectors[raw.TrackID] = in
	}
	gaps, err := in.Inspect(raw)
	if err != nil {
		return err
	}
	for _, g := range gaps {
		me := g.AsError()
		if opts.OnDamage != "recover" {
			return me
		}
		*damages = append(*damages, *me)
	}
	return nil
}
