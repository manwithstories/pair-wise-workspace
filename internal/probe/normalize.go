package probe

import (
	"fmt"
	"math/bits"
	"sort"
	"strings"
)

func normalizeAudio(a *AudioInfo, p Policy) (AudioResult, []Finding, error) {
	var findings []Finding
	res := AudioResult{
		TrackID:    a.TrackID,
		Channels:   a.EntryChannels,
		Mask:       a.Mask,
		SampleRate: a.SampleRate,
		ObjectType: a.ObjectType,
	}
	add := func(code string, sev Severity, detail string) error {
		findings = append(findings, Finding{Code: code, Track: fmt.Sprintf("audio/%d", a.TrackID), Severity: sev, Detail: detail})
		if sev == Error {
			return fmt.Errorf("%w: %s", ErrRejected, code)
		}
		return nil
	}

	maskChannels := bits.OnesCount64(a.Mask)
	switch {
	case !a.HasMask || a.Mask == 0:
		if err := add("UNKNOWN_CHANNEL_MASK", Warning, "channel mask absent; entry count preserved"); err != nil {
			return res, findings, err
		}
	case a.EntryChannels > 0 && maskChannels != a.EntryChannels:
		detail := fmt.Sprintf("mask has %d channels, sample entry declares %d", maskChannels, a.EntryChannels)
		switch p.Channels {
		case Reject:
			if err := add("CHANNEL_COUNT_CONFLICT", Error, detail); err != nil {
				return res, findings, err
			}
		case Entry:
			_ = add("CHANNEL_COUNT_CONFLICT", Warning, detail+"; entry count kept")
		default: // Mask
			res.Channels = maskChannels
			_ = add("CHANNEL_COUNT_CONFLICT", Warning, detail+"; mask used")
		}
	case maskChannels > 0:
		res.Channels = maskChannels
	}
	if a.LayoutCount > 0 && maskChannels > 0 && a.LayoutCount != maskChannels {
		detail := fmt.Sprintf("layout lists %d channels, mask popcount is %d", a.LayoutCount, maskChannels)
		switch p.Channels {
		case Reject:
			if err := add("CHANNEL_LAYOUT_CONFLICT", Error, detail); err != nil {
				return res, findings, err
			}
		case Entry:
			_ = add("CHANNEL_LAYOUT_CONFLICT", Warning, detail+"; layout ignored")
		default:
			_ = add("CHANNEL_LAYOUT_CONFLICT", Warning, detail+"; mask governs")
		}
	}

	// Sample rate.
	switch {
	case a.SampleRate <= 0 && a.Timescale > 0:
		if err := add("UNKNOWN_SAMPLE_RATE", Warning, "sample entry rate absent; timescale retained separately"); err != nil {
			return res, findings, err
		}
		res.SampleRate = a.SampleRate
	case a.SampleRate > 0 && a.Timescale > 0 && a.SampleRate != a.Timescale:
		detail := fmt.Sprintf("sample entry rate %d, media timescale %d", a.SampleRate, a.Timescale)
		switch p.SampleRate {
		case Reject:
			if err := add("SAMPLE_RATE_CONFLICT", Error, detail); err != nil {
				return res, findings, err
			}
		case Timescale:
			res.SampleRate = a.Timescale
			_ = add("SAMPLE_RATE_CONFLICT", Warning, detail+"; timescale used")
		default:
			_ = add("SAMPLE_RATE_CONFLICT", Warning, detail+"; entry rate kept")
		}
	case a.SampleRate > 0:
		res.SampleRate = a.SampleRate
	}
	res.SampleRateKnown = res.SampleRate > 0 && knownAudioRates[res.SampleRate]
	if res.SampleRate > 0 && !res.SampleRateKnown {
		if err := add("UNKNOWN_SAMPLE_RATE", Warning, fmt.Sprintf("rate %d not in the known set; preserved", res.SampleRate)); err != nil {
			return res, findings, err
		}
	}

	if a.HasObjectType {
		res.ObjectTypeKnown = knownObjectTypes[a.ObjectType]
		if !res.ObjectTypeKnown {
			if err := add("UNKNOWN_OBJECT_TYPE", Warning, fmt.Sprintf("object type 0x%02x preserved", a.ObjectType)); err != nil {
				return res, findings, err
			}
		}
	}
	return res, findings, nil
}

func normalizeVideo(v *VideoInfo, p Policy) (VideoResult, []Finding, error) {
	var findings []Finding
	res := VideoResult{TrackID: v.TrackID, Color: v.Color}
	add := func(code, detail string) {
		findings = append(findings, Finding{Code: code, Track: fmt.Sprintf("video/%d", v.TrackID), Severity: Warning, Detail: detail})
	}
	if !v.Color.Present {
		add("MISSING_COLOR", "no colr box; colour left unspecified")
		return res, findings, nil
	}
	known := true
	if !knownPrimaries[v.Color.Primaries] {
		known = false
		add("UNKNOWN_PRIMARIES", fmt.Sprintf("primaries %d preserved", v.Color.Primaries))
	}
	if !knownTransfer[v.Color.Transfer] {
		known = false
		add("UNKNOWN_TRANSFER", fmt.Sprintf("transfer %d preserved", v.Color.Transfer))
	}
	if !knownMatrix[v.Color.Matrix] {
		known = false
		add("UNKNOWN_MATRIX", fmt.Sprintf("matrix %d preserved", v.Color.Matrix))
	}
	res.ColorKnown = known
	res.Color.Complete = known
	return res, findings, nil
}

func colorKey(c ColorInfo) string {
	return fmt.Sprintf("%d/%d/%d/%t", c.Primaries, c.Transfer, c.Matrix, c.FullRange)
}

func reconcileAudio(r *Report, p Policy) error {
	first := r.Audio[0]
	for i := 1; i < len(r.Audio); i++ {
		a := r.Audio[i]
		if first.SampleRate != a.SampleRate {
			detail := fmt.Sprintf("track %d rate %d, track %d rate %d", first.TrackID, first.SampleRate, a.TrackID, a.SampleRate)
			switch p.SampleRate {
			case Reject:
				r.Findings = append(r.Findings, Finding{Code: "SAMPLE_RATE_CONFLICT", Severity: Error, Detail: detail})
				return fmt.Errorf("%w: sample rate across representations", ErrRejected)
			case Timescale, Entry:
				// Both are per-track decisions; across representations the
				// first representation is authoritative for the programme.
				r.Findings = append(r.Findings, Finding{Code: "SAMPLE_RATE_CONFLICT", Severity: Warning, Detail: detail + "; first representation used"})
				a.SampleRate = first.SampleRate
			}
		}
		if first.Mask != a.Mask {
			detail := fmt.Sprintf("track %d mask 0x%x, track %d mask 0x%x", first.TrackID, first.Mask, a.TrackID, a.Mask)
			if p.Channels == Reject {
				r.Findings = append(r.Findings, Finding{Code: "CHANNEL_MASK_CONFLICT", Severity: Error, Detail: detail})
				return fmt.Errorf("%w: channel mask across representations", ErrRejected)
			}
			r.Findings = append(r.Findings, Finding{Code: "CHANNEL_MASK_CONFLICT", Severity: Warning, Detail: detail + "; first representation used"})
			a.Mask = first.Mask
			a.Channels = first.Channels
		}
	}
	r.Audio = r.Audio[:1]
	return nil
}

func reconcileVideo(r *Report, p Policy) error {
	first := r.Video[0]
	var conflicts []string
	for i := 1; i < len(r.Video); i++ {
		if colorKey(r.Video[i].Color) != colorKey(first.Color) {
			conflicts = append(conflicts, fmt.Sprintf("track %d %s", r.Video[i].TrackID, colorKey(r.Video[i].Color)))
		}
	}
	if len(conflicts) == 0 {
		r.Video = r.Video[:1]
		return nil
	}
	sort.Strings(conflicts)
	detail := fmt.Sprintf("primary %d %s; differing %s", first.TrackID, colorKey(first.Color), strings.Join(conflicts, ", "))
	if p.Color == Reject {
		r.Findings = append(r.Findings, Finding{Code: "COLOR_CONFLICT", Severity: Error, Detail: detail})
		return fmt.Errorf("%w: colour metadata across representations", ErrRejected)
	}
	r.Findings = append(r.Findings, Finding{Code: "COLOR_CONFLICT", Severity: Warning, Detail: detail + "; first representation used"})
	r.Video = r.Video[:1]
	return nil
}
