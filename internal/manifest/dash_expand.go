package manifest

import (
	"strconv"
	"strings"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

// expandRepresentation turns a resolved template into segment records.
// periodDurCanon is the Period duration in canonical ticks, or -1.
func expandRepresentation(t *Template, rep Representation, periodDurCanon int64, canon clock.Timebase) ([]Segment, []fault.Fault) {
	var faults []fault.Fault
	native := clock.Timebase{Timescale: t.Timescale}
	if !native.Valid() {
		return nil, []fault.Fault{fault.MustNew(fault.KindManifestParse,
			fault.Location{Stream: rep.ID, Offset: -1}, "invalid Template@timescale", true, false)}
	}
	pto := t.PresentationTimeOffset
	number := t.StartNumber
	if !t.HasStartNumber {
		number = 1
	}
	var out []Segment
	if len(t.Timeline) > 0 {
		periodEndNative := int64(-1)
		if periodDurCanon >= 0 {
			periodEndNative = pto + canon.Convert(periodDurCanon, native)
		}
		var nextNative int64
		for _, e := range t.Timeline {
			start := nextNative
			if e.HasTime {
				start = e.Time
			}
			if e.Duration <= 0 {
				faults = append(faults, fault.MustNew(fault.KindManifestParse,
					fault.Location{Stream: rep.ID, Offset: -1}, "SegmentTimeline S@d must be positive", true, false))
				return out, faults
			}
			repeat := e.Repeat
			if repeat < 0 {
				if periodEndNative >= 0 {
					// r=-1 repeats until the Period ends; the segment that
					// would start exactly at the end boundary is excluded.
					span := periodEndNative - start
					if span <= 0 {
						repeat = 0
					} else {
						repeat = (span - 1) / e.Duration
					}
				} else {
					faults = append(faults, fault.MustNew(fault.KindMissingIndex,
						fault.Location{Stream: rep.ID, Offset: -1}, "S@r=-1 without known Period duration", true, false))
					repeat = 0
				}
			}
			for k := int64(0); k <= repeat; k++ {
				nativeStart := start + k*e.Duration
				out = append(out, Segment{
					URI:             expandTemplate(t.Media, rep, number, uint64(nativeStart)),
					InitURI:         rep.InitURI,
					Sequence:        number,
					Start:           canonTicks(nativeStart-pto, native, canon),
					Duration:        canonTicks(e.Duration, native, canon),
					NativeTimescale: t.Timescale,
					NativeStart:     nativeStart,
					NativeDuration:  e.Duration,
					DurationKnown:   true,
				})
				number++
			}
			nextNative = start + (repeat+1)*e.Duration
		}
		return out, faults
	}
	if t.HasDuration && t.Duration > 0 {
		if periodDurCanon < 0 {
			faults = append(faults, fault.MustNew(fault.KindMissingIndex,
				fault.Location{Stream: rep.ID, Offset: -1}, "SegmentTemplate duration without known Period duration", true, false))
			return out, faults
		}
		periodNative := canon.Convert(periodDurCanon, native)
		count := (periodNative + t.Duration - 1) / t.Duration
		if pto > 0 && periodNative > pto {
			count = (periodNative - pto + t.Duration - 1) / t.Duration
		}
		for i := int64(0); i < count; i++ {
			nativeStart := pto + i*t.Duration
			out = append(out, Segment{
				URI:             expandTemplate(t.Media, rep, number, uint64(nativeStart)),
				InitURI:         rep.InitURI,
				Sequence:        number,
				Start:           canonTicks(i*t.Duration, native, canon),
				Duration:        canonTicks(t.Duration, native, canon),
				NativeTimescale: t.Timescale,
				NativeStart:     nativeStart,
				NativeDuration:  t.Duration,
				DurationKnown:   true,
			})
			number++
		}
		return out, faults
	}
	faults = append(faults, fault.MustNew(fault.KindMissingIndex,
		fault.Location{Stream: rep.ID, Offset: -1}, "template has neither SegmentTimeline nor duration", true, false))
	return out, faults
}

func canonTicks(v int64, native, canon clock.Timebase) int64 {
	if v <= 0 {
		return 0
	}
	return native.Convert(v, canon)
}

// expandTemplate substitutes $RepresentationID$, $Number$, $Time$ and
// $Bandwidth$ with optional %0Nd formatting.
func expandTemplate(tmpl string, rep Representation, number uint64, mediaTime uint64) string {
	if tmpl == "" {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		j := strings.IndexByte(tmpl[i:], '$')
		if j < 0 {
			b.WriteString(tmpl[i:])
			break
		}
		b.WriteString(tmpl[i : i+j])
		i += j
		if i+1 < len(tmpl) && tmpl[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		end := strings.IndexByte(tmpl[i+1:], '$')
		if end < 0 {
			b.WriteString(tmpl[i:])
			break
		}
		spec := tmpl[i+1 : i+1+end]
		token, format := spec, ""
		if p := strings.IndexByte(spec, '%'); p >= 0 {
			token, format = spec[:p], spec[p:]
		}
		var val string
		switch token {
		case "RepresentationID":
			val = rep.ID
		case "Number":
			val = formatID(number, format)
		case "Time":
			val = formatID(mediaTime, format)
		case "Bandwidth":
			val = formatID(uint64(rep.Bandwidth), format)
		default:
			b.WriteString(tmpl[i : i+end+2])
			i += end + 2
			continue
		}
		b.WriteString(val)
		i += end + 2
	}
	return b.String()
}

// formatID applies a %0Nd identifier format, ignoring anything else so a
// malformed template can never alter the output unexpectedly.
func formatID(v uint64, format string) string {
	s := strconv.FormatUint(v, 10)
	if len(format) < 3 || format[0] != '%' || format[1] != '0' || format[len(format)-1] != 'd' {
		return s
	}
	width, err := strconv.Atoi(format[2 : len(format)-1])
	if err != nil || width < 0 || width > 20 {
		return s
	}
	for len(s) < width {
		s = "0" + s
	}
	return s
}
