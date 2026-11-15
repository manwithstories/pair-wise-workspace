package main

import (
	"fmt"
	"math"

	"loudcheck/internal/loudness"
	"loudcheck/internal/wavio"
)

func runVerify(args []string) error {
	flags := parseFlags(args)
	seed := uint64(7)
	if value, ok := flags["seed"]; ok {
		fmt.Sscanf(value, "%d", &seed)
	}
	header := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 32, IsFloat: true}
	gen := &fixtureGenerator{seed: seed}
	tone := gen.calibratedTone(48000, 20, 2, 1000, -23, header)
	toneMetric, err := measurePlanar(header, tone)
	if err != nil {
		return err
	}
	pinkHeader := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 32, IsFloat: true}
	pink := gen.calibratedPink(48000, 20, 2, -23, pinkHeader)
	pinkMetric, err := measurePlanar(pinkHeader, pink)
	if err != nil {
		return err
	}
	checks := []struct {
		name   string
		got    float64
		target float64
		tol    float64
	}{
		{"tone integrated", toneMetric.IntegratedLUFS, -23, 0.1},
		{"pink integrated", pinkMetric.IntegratedLUFS, -23, 0.1},
	}
	failed := false
	for _, check := range checks {
		delta := math.Abs(check.got - check.target)
		status := "OK"
		if delta > check.tol {
			status = "FAIL"
			failed = true
		}
		fmt.Printf("verify %s: got %.3f want %.3f delta %.3f LU [%s]\n",
			check.name, check.got, check.target, delta, status)
	}
	if !checkIntersamplePeak() {
		fmt.Println("verify intersample peak: FAIL")
		failed = true
	} else {
		fmt.Println("verify intersample peak: OK")
	}
	if err := checkSilenceUndefined(); err != nil {
		fmt.Println("verify silence undefined: FAIL", err)
		failed = true
	} else {
		fmt.Println("verify silence undefined: OK")
	}
	if failed {
		return &exitError{code: exitFail}
	}
	fmt.Println("all verify checks passed")
	return nil
}

func checkIntersamplePeak() bool {
	header := wavio.WriteHeader{SampleRate: 48000, Channels: 1, BitsPerSample: 32, IsFloat: true}
	planar := wavio.TonePlanar(48000, 4, 1, 15750, 0.5, 0)
	metric, err := measurePlanar(header, planar)
	if err != nil {
		return false
	}
	return metric.InterSampleOvershoot && metric.OversampledPeak > metric.SamplePeakLinear
}

func checkSilenceUndefined() error {
	header := wavio.WriteHeader{SampleRate: 48000, Channels: 1, BitsPerSample: 16}
	planar := make([][]float64, 1)
	planar[0] = make([]float64, 48000)
	metric, err := measurePlanar(header, planar)
	if err != nil {
		return err
	}
	if metric.Defined || !math.IsNaN(metric.IntegratedLUFS) {
		return fmt.Errorf("silence produced defined loudness")
	}
	return nil
}

var _ = loudness.AbsoluteGateLUFS
