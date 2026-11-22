package subtitle

import (
	"strings"
)

// Records returns the normalized records used for round-trip comparison. Raw
// timestamp tokens are excluded because a legal re-serialization may use a
// different but equivalent precision; numeric times and text bytes must match.
func (d *Document) Records() []Cue {
	out := make([]Cue, len(d.Cues))
	for i, c := range d.Cues {
		c.StartRaw, c.EndRaw = "", ""
		c.LineOffset = 0
		out[i] = c
	}
	return out
}

// EqualRecords reports whether two documents carry identical normalized
// records: cue count, indices, nanosecond spans, identifiers, settings and
// text bytes.
func EqualRecords(a, b *Document) bool {
	if a == nil || b == nil || len(a.Cues) != len(b.Cues) {
		return false
	}
	for i := range a.Cues {
		x, y := a.Cues[i], b.Cues[i]
		if x.Index != y.Index || x.StartNs != y.StartNs || x.EndNs != y.EndNs ||
			x.Text != y.Text || x.Identifier != y.Identifier || x.Settings != y.Settings {
			return false
		}
	}
	return true
}

// Serialize renders the document back to its source format. For well-formed
// input the output reparses to identical normalized records.
func Serialize(d *Document) string {
	var b strings.Builder
	switch d.Format {
	case FormatVTT:
		b.WriteString(serializeVTT(d))
	default:
		b.WriteString(serializeSRT(d))
	}
	return b.String()
}

func serializeSRT(d *Document) string {
	var b strings.Builder
	for i, c := range d.Cues {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(itoa(c.Index))
		b.WriteByte('\n')
		b.WriteString(c.StartRaw)
		b.WriteString(" --> ")
		b.WriteString(c.EndRaw)
		b.WriteByte('\n')
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func serializeVTT(d *Document) string {
	var b strings.Builder
	if d.Header != "" {
		b.WriteString(d.Header)
		b.WriteByte('\n')
	}
	for i, c := range d.Cues {
		if i > 0 {
			b.WriteByte('\n')
		}
		if c.Identifier != "" {
			b.WriteString(c.Identifier)
			b.WriteByte('\n')
		}
		b.WriteString(c.StartRaw)
		b.WriteString(" --> ")
		b.WriteString(c.EndRaw)
		if c.Settings != "" {
			b.WriteByte(' ')
			b.WriteString(c.Settings)
		}
		b.WriteByte('\n')
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
