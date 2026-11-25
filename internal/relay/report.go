package relay

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

func printReport(w io.Writer, rep *Report) {
	order := []string{
		"roundtrip_rate", "keyframe_hits", "frames", "segments",
		"sha256_match", "wraps", "vfr_intervals", "recovered", "cancelled",
	}
	vals := map[string]string{
		"roundtrip_rate": rep.RoundtripRate,
		"keyframe_hits":  rep.Keyframe,
		"frames":         strconv.Itoa(rep.Frames),
		"segments":       strconv.Itoa(rep.Segments),
		"sha256_match":   rep.SHA256Match,
		"wraps":          strconv.Itoa(rep.Wraps),
		"vfr_intervals":  strconv.Itoa(rep.VFRIntervals),
		"recovered":      rep.Recovered,
		"cancelled":      strconv.Itoa(rep.Cancelled),
	}
	for _, k := range order {
		fmt.Fprintf(w, "%s=%s\n", k, vals[k])
	}
	for _, c := range rep.Conflicts {
		losers := make([]string, 0, len(c.Losers))
		for _, l := range c.Losers {
			losers = append(losers, fmt.Sprintf("%s:%s", l.Source, l.Value))
		}
		fmt.Fprintf(w, "conflict=%s field=%s chose=%s:%s rejected=%s\n",
			c.TrackID, c.Field, c.Chosen.Source, c.Chosen.Value, strings.Join(losers, ","))
	}
	for _, it := range rep.Faults.Sorted() {
		fmt.Fprintf(w, "fault=%s\n", it)
	}
	for _, s := range rep.Shards {
		fmt.Fprintf(w, "png_shard=%d.%s\n", s.Index, s.Hash)
	}
	fmt.Fprintf(w, "exit=%d\n", rep.Exit)
}
