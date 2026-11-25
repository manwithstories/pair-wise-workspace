package main

import (
	"fmt"

	"mediaaudit/internal/fixture"
)

func runFixture(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	dir, err := f.required("dir")
	if err != nil {
		return err
	}
	seed, err := f.int64("seed", 1)
	if err != nil {
		return err
	}
	channels, err := f.int64("channels", 6)
	if err != nil {
		return err
	}
	frames, err := f.int64("frames", 480000)
	if err != nil {
		return err
	}
	tile, err := f.int64("tile-frames", 32)
	if err != nil {
		return err
	}
	damage, err := f.int64("damage-frame", -1)
	if err != nil {
		return err
	}
	wrapAt, err := f.uint32("wrap-at", 0)
	if err != nil {
		return err
	}
	drift, err := f.float64("drift-ppm", 0)
	if err != nil {
		return err
	}
	vfr, err := f.intList("vfr")
	if err != nil {
		return err
	}
	if channels <= 0 || channels > 32 {
		return fmt.Errorf("channels must be in 1..32")
	}
	o := fixture.Options{
		Dir: dir, Seed: seed, Channels: int(channels), Frames: frames,
		SampleRate: 48000, TileFrames: tile, DamageFrame: damage,
		WrapAt: wrapAt, DriftPPM: drift, VFRPattern: vfr,
	}
	m, err := fixture.Build(o)
	if err != nil {
		return err
	}
	fmt.Printf("fixture written to %s\n", dir)
	fmt.Printf("  files: %v\n", m.Files)
	fmt.Printf("  fingerprint=%s damage_count=%d drift_ppm=%.0f\n",
		m.Fingerprint, m.DamageCount, m.DriftPPM)
	return nil
}
