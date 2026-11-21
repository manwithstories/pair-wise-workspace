package main

import (
	"context"
	"errors"

	"mediacore/internal/mux"
	"mediacore/internal/pipeline"
)

// cycleInterrupt performs the first half of a two-phase demo: copy samples
// until the interrupt point, commit a checkpoint, and stop. The caller then
// resumes from the verified checkpoint in the same process.
func (st *runState) cycleInterrupt(f *demoFlags) error {
	q := pipeline.NewQueue(st.src, pipeline.Limits{
		MaxSamples: f.queueSamples,
		MaxBytes:   f.queueBytes,
	})
	r := &mux.Remuxer{
		Tracks:   st.container.Tracks,
		Plan:     st.plan.Outputs(),
		Resolved: st.resolvedMeta(),
		Source:   st.src,
	}
	_, err := r.Run(context.Background(), mux.Options{
		OutputPath:     st.outputPath,
		CheckpointPath: f.checkpoint,
		InterruptAfter: f.interruptAfter,
		Fingerprint:    st.container.Fingerprint,
		MappingSign:    st.plan.Sign(),
		TotalSamples:   totalSamples(st.container),
		WaveSize:       waveSize(f),
		Queue:          q,
		QueuePeak:      &st.queuePeak,
		ConsumerDelay:  f.consumerDelay,
	})
	if err == nil {
		return errors.New("interrupt cycle ran to completion without interrupting")
	}
	var ie *mux.InterruptError
	if !errors.As(err, &ie) {
		return err
	}
	return nil
}
