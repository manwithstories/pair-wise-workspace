package scan

import (
	"fmt"
	"os"
	"testing"
)

// TestLiveFiles is a developer probe over externally generated files. It is
// skipped unless MEDIACORE_LIVE_FILES lists colon-separated paths.
func TestLiveFiles(t *testing.T) {
	list := os.Getenv("MEDIACORE_LIVE_FILES")
	if list == "" {
		t.Skip("MEDIACORE_LIVE_FILES not set")
	}
	for _, p := range splitPath(list) {
		s, err := Run(p, Options{})
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		fmt.Printf("%s format=%s tracks=%d kf=%d meta=%d chapters=%d packets=%d\n",
			p, s.Format, len(s.Tracks), len(s.Keyframes), len(s.Meta), len(s.Chapters), s.Packets)
		for _, tr := range s.Tracks {
			fmt.Printf("  track %d %s %s tb=%s start=%d\n", tr.Index, tr.Kind, tr.Codec, tr.TimeBase, tr.Start)
		}
		for i, m := range s.Meta {
			if i < 8 {
				fmt.Printf("  meta %s track=%d t=%d text=%q quar=%v reason=%q\n", m.Kind, m.Track, m.TimeUS, m.Text, m.Quarantined, m.Reason)
			}
		}
		for _, c := range s.Chapters {
			fmt.Printf("  chapter %d %q %d-%d\n", c.ID, c.Title, c.StartUs, c.EndUs)
		}
	}
}

func splitPath(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ':' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
