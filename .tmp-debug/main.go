package main

import (
	"fmt"
	"os"
	"mediaroute/internal/fixture"
	"mediaroute/internal/mp4"
)

func main() {
	dir, _ := os.MkdirTemp("", "fx")
	fixture.Generate(fixture.Spec{Seed: 7, Minutes: 6, Cameras: 4, Bitrates: []uint32{100, 200, 400}, Dir: dir})
	rt, err := mp4.ScanFile(dir + "/media/cam2_100.mp4")
	if err != nil { panic(err) }
	fmt.Println("stts:", rt.Stts)
	fmt.Println("sizes:", len(rt.SampleSizes), "timescale:", rt.Timescale)
	s := rt.Expand()
	for i := 90; i < 100; i++ {
		fmt.Printf("sample %d dts=%d\n", i, s[i].DTS)
	}
}
