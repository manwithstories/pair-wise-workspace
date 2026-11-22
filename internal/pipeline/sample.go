package pipeline

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"

	"mediasync/internal/clock"
	"mediasync/internal/ingest"
	"mediasync/internal/subtitle"
	"mediasync/internal/timeline"
)

// Sample records carried as RTP payloads:
//   C|<startNanos>|<endNanos>|<text>      subtitle cue
//   M|<rateNum>/<rateDen>|<HH:MM:SS:FF>|<label>  timecode marker
//   K|nop                                 keepalive

// generateSRT builds a deterministic SRT document of n cues. When
// corrupt is true the first digit of the middle cue's timing line is
// raised so its start jumps past its end; the returned offset is the
// exact byte the parser must locate.
func generateSRT(n int, seed int64, corrupt bool) ([]byte, int64) {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	rng := rand.New(rand.NewSource(seed))
	var b strings.Builder
	bad := int64(-1)
	for i := 0; i < n; i++ {
		start := int64(i) * 2 * clock.NanosPerSec
		end := start + 2_500_000_000
		fmt.Fprintf(&b, "%d\n", i+1)
		if i == n/2 {
			bad = int64(b.Len())
		}
		fmt.Fprintf(&b, "%s --> %s\n", srtTS(start), srtTS(end))
		fmt.Fprintf(&b, "cue-%06d %s\n\n", i, words[rng.Intn(len(words))])
	}
	src := []byte(b.String())
	if corrupt && bad >= 0 {
		src[bad] = '1' // hours tens 0 -> 1: start 10h ahead of end
	}
	return src, bad
}

func srtTS(ns int64) string {
	h := ns / 3_600_000_000_000
	m := ns / 60_000_000_000 % 60
	s := ns / 1_000_000_000 % 60
	ms := ns % 1_000_000_000 / 1_000_000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

func formatTC(r clock.Rate, ns int64) string {
	f := r.NanosToFrame(ns)
	nom := r.NominalFPS()
	s := f / nom
	return fmt.Sprintf("%02d:%02d:%02d:%02d", s/3600, (s/60)%60, s%60, f%nom)
}

// buildDatagrams encodes cues, alternating-rate timecode markers and
// keepalive padding into exactly packets RTP datagrams.
func buildDatagrams(cues []subtitle.Cue, packets int) ([][]byte, error) {
	var recs [][]byte
	for _, c := range cues {
		text := strings.NewReplacer("\n", " / ", "|", "/").Replace(c.Text)
		recs = append(recs, []byte(fmt.Sprintf("C|%d|%d|%s", c.Start, c.End, text)))
	}
	for k := 0; k < packets/markerEvery; k++ {
		rate := clock.FPS24
		if k%2 == 1 {
			rate = clock.FPS2997
		}
		ns := int64(k) * 10 * clock.NanosPerSec
		recs = append(recs, []byte(fmt.Sprintf("M|%s|%s|mark-%04d", rate, formatTC(rate, ns), k)))
	}
	for len(recs) < packets {
		recs = append(recs, []byte("K|nop"))
	}
	recs = recs[:packets]
	out := make([][]byte, len(recs))
	for i, rec := range recs {
		b, err := ingest.Encode(uint16(i), uint32(i*160), ssrc, rec)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// impair simulates the link: a burst loss, periodic duplicates and
// adjacent-packet reordering, all deterministic.
func impair(datagrams [][]byte, lossBurst int, seed int64) [][]byte {
	l0 := len(datagrams)*2/5 + int(seed%7)
	var out [][]byte
	for i, d := range datagrams {
		if lossBurst > 0 && i >= l0 && i < l0+lossBurst {
			continue
		}
		out = append(out, d)
		if len(out)%dupEvery == 0 {
			out = append(out, d) // duplicate
		}
	}
	for j := 0; j+1 < len(out); j += swapEvery {
		out[j], out[j+1] = out[j+1], out[j] // reorder
	}
	return out
}

func chunkify(datagrams [][]byte, size int) [][][]byte {
	var out [][][]byte
	for i := 0; i < len(datagrams); i += size {
		j := i + size
		if j > len(datagrams) {
			j = len(datagrams)
		}
		out = append(out, datagrams[i:j])
	}
	return out
}

func parseRecord(p []byte) (*subtitle.Cue, *timeline.Marker, error) {
	s := string(p)
	switch {
	case strings.HasPrefix(s, "K|"):
		return nil, nil, nil
	case strings.HasPrefix(s, "C|"):
		parts := strings.SplitN(s, "|", 4)
		if len(parts) != 4 {
			return nil, nil, fmt.Errorf("bad cue record %q", s)
		}
		st, err1 := strconv.ParseInt(parts[1], 10, 64)
		en, err2 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil {
			return nil, nil, fmt.Errorf("bad cue record %q", s)
		}
		return &subtitle.Cue{Start: st, End: en, Text: parts[3]}, nil, nil
	case strings.HasPrefix(s, "M|"):
		parts := strings.SplitN(s, "|", 4)
		if len(parts) != 4 {
			return nil, nil, fmt.Errorf("bad marker record %q", s)
		}
		var rate clock.Rate
		if _, err := fmt.Sscanf(parts[1], "%d/%d", &rate.Num, &rate.Den); err != nil {
			return nil, nil, fmt.Errorf("bad marker rate %q", parts[1])
		}
		tc, err := clock.ParseTimecode(parts[2], rate)
		if err != nil {
			return nil, nil, err
		}
		return nil, &timeline.Marker{Rate: rate, Nanos: tc.Nanos(), Label: parts[3]}, nil
	}
	return nil, nil, fmt.Errorf("unknown record %q", s)
}
