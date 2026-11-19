package main

import (
	"fmt"
	"os"

	"aircheck/internal/sample"
	"aircheck/internal/scan"
)

func main() {
	dir, _ := os.MkdirTemp("", "dbg")
	files, err := sample.Generate(42, dir)
	if err != nil { panic(err) }
	for _, sf := range files {
		f, _ := os.Open(sf.Path)
		st, _ := f.Stat()
		ci, err := scan.Parse(f, st.Size())
		fmt.Println("==", sf.Kind, "err:", err, "damage:", ci.DamageOffset)
		for _, t := range ci.Tracks {
			pkts, fl := scan.BuildPackets(t, st.Size())
			keys := 0
			for _, p := range pkts { if p.Keyframe { keys++ } }
			fmt.Printf("  track %d %s ts=%d samples=%d sync=%v pkts=%d keys=%d gaps=%v faults=%d\n",
				t.ID, t.MediaType, t.Timescale, len(t.SampleSizes), t.SyncSamples, len(pkts), keys,
				scan.KeyframeGaps(pkts, 12), len(fl))
		}
		f.Close()
	}
}
