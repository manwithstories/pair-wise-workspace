package recovery

func (e *exec) stageOutput() error {
	audioPath := e.outputPath("audio.wav")
	if err := writeFileAtomic(audioPath, e.master); err != nil {
		return err
	}
	e.sum.MasterOutputSHA = HashBytes(e.master)
	return e.finishStage("output")
}
