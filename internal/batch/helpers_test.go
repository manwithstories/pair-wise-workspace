package batch

import "loudcheck/internal/loudness"

func fakeResult(integrated, peak float64, defined bool) loudness.EngineResult {
	return loudness.EngineResult{
		GateResult: loudness.GateResult{
			IntegratedLUFS: integrated,
			Defined:        defined,
		},
		TruePeakDBTP: peak,
	}
}
