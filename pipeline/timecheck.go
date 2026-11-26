package pipeline

import (
	"medcheck/clock"
	"medcheck/fault"
)

// checkGeneratedTimecodes exercises 24/25/30/NDF and 30000/1001 DF validation,
// including a legal midnight wrap, an illegal DF number and a regression.
func checkGeneratedTimecodes(fm *fault.Model) (invalidDF int, midnightWrap bool) {
	prev, err := clock.ParseTimecode("23:59:59:24", clock.Rate25, false)
	if err == nil {
		next, perr := clock.ParseTimecode("00:00:00:00", clock.Rate25, false)
		if perr == nil {
			tr, terr := clock.CheckTransition(prev, next)
			if terr == nil {
				midnightWrap = tr.CrossMidnight
			}
		}
	}
	if !midnightWrap {
		fm.Add(fault.Fault{Kind: fault.InvalidTimecode, Message: "generated cross-midnight transition was rejected"})
	}

	if _, err := clock.ParseTimecode("00:01:00;00", clock.Rate30000_1001, true); err != nil {
		invalidDF++
		fm.Add(fault.Fault{Kind: fault.InvalidTimecode, Message: "illegal DF frame 00:01:00;00"})
	}

	backPrev, _ := clock.ParseTimecode("01:00:00:00", clock.Rate30, false)
	backNext, _ := clock.ParseTimecode("00:59:59:29", clock.Rate30, false)
	if _, err := clock.CheckTransition(backPrev, backNext); err != nil {
		fm.Add(fault.Fault{Kind: fault.InvalidTimecode, Message: "generated timecode regression"})
	}

	ltc, _ := clock.ParseTimecode("01:00:00:00", clock.Rate24, false)
	vitc, _ := clock.ParseTimecode("01:00:00:01", clock.Rate24, false)
	if align, err := clock.CheckAlignment(ltc, vitc, 1); err != nil || !align.Aligned {
		fm.Add(fault.Fault{Kind: fault.InvalidTimecode, Message: "LTC/VITC alignment failed"})
	}
	return invalidDF, midnightWrap
}
