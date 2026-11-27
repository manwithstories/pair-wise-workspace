package clock

import (
	"math"
	"strconv"
)

// DefaultDriftPPMLimit is the per-tract clock tolerance: 1000 ppm would be
// pathological; real live edges stay well inside 250 ppm.
const DefaultDriftPPMLimit = 250.0

// DriftEstimator tracks one track's node clock against the shared master
// using an EWMA of the observed rate (slave units per master millisecond).
// It reports drift in parts-per-million and rejects observations past the
// configured bound instead of hiding them.
type DriftEstimator struct {
	nominalRate float64 // expected slave units per master ms (timescale/1000)
	alpha       float64 // EWMA weight for a fresh sample
	ppmLimit    float64
	rate        float64
	have        bool
	lastMaster  int64
	lastSlave   uint64
	DriftPPM    float64
	Samples     int
}

// NewDriftEstimator builds an estimator for a track with the given timescale.
// masterTimescale is the units-per-second of the shared clock (1000 => ms).
func NewDriftEstimator(timescale, masterTimescale uint32) *DriftEstimator {
	if masterTimescale == 0 {
		masterTimescale = 1000
	}
	nominal := float64(timescale) / float64(masterTimescale)
	return &DriftEstimator{
		nominalRate: nominal,
		alpha:       0.2,
		ppmLimit:    DefaultDriftPPMLimit,
	}
}

// Observe feeds one (masterMs, slaveTimeTicks) pair. A non-positive master
// delta is ignored; a slave rollback is surfaced as an error. The returned
// confidence is Rejected when |drift| exceeds the bound.
func (d *DriftEstimator) Observe(masterMs int64, slaveTicks uint64) (Confidence, error) {
	if !d.have {
		d.have = true
		d.lastMaster, d.lastSlave = masterMs, slaveTicks
		d.rate = d.nominalRate
		return Confirmed, nil
	}
	dm := masterMs - d.lastMaster
	ds := int64(slaveTicks - d.lastSlave)
	if dm <= 0 {
		return Rejected, &TimelineError{Kind: ErrDriftExceeded, At: masterMs,
			Detail: "non-positive master interval"}
	}
	if ds < 0 {
		return Rejected, &TimelineError{Kind: ErrTfdtRollback, At: masterMs,
			Detail: "slave clock moved backwards"}
	}
	instRate := float64(ds) / float64(dm)
	d.rate = (1-d.alpha)*d.rate + d.alpha*instRate
	if d.nominalRate > 0 {
		d.DriftPPM = (d.rate/d.nominalRate - 1) * 1e6
	}
	d.Samples++
	d.lastMaster, d.lastSlave = masterMs, slaveTicks
	if math.Abs(d.DriftPPM) > d.ppmLimit {
		return Rejected, &TimelineError{Kind: ErrDriftExceeded, At: masterMs,
			Detail: ppmText(d.DriftPPM, d.ppmLimit)}
	}
	if d.Samples == 1 {
		return Confirmed, nil
	}
	// A measured (non-ideal) rate is a repair-grade input.
	return Uncertain, nil
}

// Rate returns the current smoothed slave-units-per-master-ms rate.
func (d *DriftEstimator) Rate() float64 {
	if !d.have {
		return d.nominalRate
	}
	return d.rate
}

func ppmText(got, limit float64) string {
	return "drift " + strconv.FormatFloat(got, 'f', 1, 64) +
		"ppm exceeds limit " + strconv.FormatFloat(limit, 'f', 1, 64) + "ppm"
}
