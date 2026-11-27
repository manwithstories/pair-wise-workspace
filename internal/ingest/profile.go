package ingest

import (
	"fmt"
	"math/rand"

	"mediacheck/internal/model"
)

// Profile names the sample scenarios the demo command exposes.
type Profile string

const (
	// ProfileClean passes every packet through untouched.
	ProfileClean Profile = "clean"
	// ProfileDamaged drops audio packets, duplicates a few and regresses a
	// timestamp.
	ProfileDamaged Profile = "damaged"
	// ProfileTruncated cuts payloads in the middle of NAL units and audio
	// frames so the parser must localise damage without losing later
	// packets.
	ProfileTruncated Profile = "truncated"
	// ProfileWrap starts near the 16 bit sequence boundary so the counter
	// wraps and a small gap follows it.
	ProfileWrap Profile = "wrap"
	// ProfileSlow is clean data consumed by a deliberately slow reader.
	ProfileSlow Profile = "slow"
)

// ParseProfile validates a profile name from the command line.
func ParseProfile(s string) (Profile, error) {
	switch Profile(s) {
	case ProfileClean, ProfileDamaged, ProfileTruncated, ProfileWrap, ProfileSlow:
		return Profile(s), nil
	default:
		return "", fmt.Errorf("ingest: unknown profile %q", s)
	}
}

// AllProfiles lists the demo profiles.
func AllProfiles() []Profile {
	return []Profile{ProfileClean, ProfileDamaged, ProfileTruncated, ProfileWrap, ProfileSlow}
}

// Injected records what a profile changed, so tests can assert that every
// intended fault was actually created.
type Injected struct {
	Dropped      []model.Slot
	Duplicated   []model.Slot
	Truncated    []model.Slot
	Regressed    []model.Slot
	SequenceBase uint32
}

// ApplyProfile corrupts g in place according to profile.  The mutations are
// deterministic functions of cfg.Seed.
func (g *Generated) ApplyProfile(profile Profile) (Injected, error) {
	switch profile {
	case ProfileClean, ProfileSlow:
		return Injected{}, nil
	case ProfileDamaged:
		return g.injectDamaged(), nil
	case ProfileTruncated:
		return g.injectTruncated(), nil
	case ProfileWrap:
		return g.injectWrap()
	default:
		return Injected{}, fmt.Errorf("ingest: unknown profile %q", profile)
	}
}

// injectDamaged drops every 40th audio packet, duplicates every 97th and
// pulls one timestamp backwards.  Audio loss is what recover can fill.
func (g *Generated) injectDamaged() Injected {
	rng := rand.New(rand.NewSource(g.Config.Seed ^ 0x5eed))
	var inj Injected
	var kept []model.Packet
	for i, p := range g.Audio.Packets {
		switch {
		case i > 0 && i%40 == 0:
			inj.Dropped = append(inj.Dropped, p.Slot)
			continue
		case i > 0 && i%97 == 0:
			inj.Duplicated = append(inj.Duplicated, p.Slot)
			kept = append(kept, p, p.Clone())
			continue
		}
		kept = append(kept, p)
	}
	g.Audio.Packets = kept
	// Regress one presentation timestamp on the video track by two frames
	// and leave sequence numbers contiguous, so the reader sees a pure
	// timestamp fault.
	if len(g.Video.Packets) > 60 {
		idx := 40 + rng.Intn(10)
		g.Video.Packets[idx].PTS -= 2
		inj.Regressed = append(inj.Regressed, g.Video.Packets[idx].Slot)
	}
	return inj
}

// injectTruncated cuts the payload of selected audio and video packets.  The
// sequence numbers stay contiguous: later packets must still be found.
func (g *Generated) injectTruncated() Injected {
	var inj Injected
	for i := range g.Audio.Packets {
		if i > 0 && i%53 == 0 {
			p := &g.Audio.Packets[i]
			p.Data = p.Data[:len(p.Data)/3]
			inj.Truncated = append(inj.Truncated, p.Slot)
		}
	}
	for i := range g.Video.Packets {
		if i > 0 && i%29 == 0 {
			p := &g.Video.Packets[i]
			// Keep the first length prefix intact but cut the NAL body so
			// the parser sees a declared length beyond the buffer.
			p.Data = p.Data[:len(p.Data)/2]
			inj.Truncated = append(inj.Truncated, p.Slot)
		}
	}
	return inj
}

// injectWrap renumbers audio sequences to start just below 65536 so the
// counter wraps during the run, and removes three packets right after it.
func (g *Generated) injectWrap() (Injected, error) {
	const base = 65500
	var inj Injected
	renumbered := make([]model.Packet, 0, len(g.Audio.Packets))
	for i, p := range g.Audio.Packets {
		p.Sequence = uint32((base + i) % seqWindow)
		if i >= 40 && i < 44 {
			inj.Dropped = append(inj.Dropped, p.Slot)
			continue
		}
		renumbered = append(renumbered, p)
	}
	g.Audio.Packets = renumbered
	inj.SequenceBase = base
	return inj, nil
}
