package main

import (
	"os"

	"mediad/internal/bmff"
	"mediad/internal/model"
	"mediad/internal/pipeline"
	"mediad/internal/trackmap"
)

func removeIfExists(paths ...string) error {
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return model.Wrap(model.CodeInternal, model.StageCLI, "reset demo artifact", err).WithSource(p)
		}
	}
	return nil
}

func demoMapping() []trackmap.Request {
	return []trackmap.Request{
		{SourceID: 1, OutputID: 1},
		{SourceID: 2, OutputID: 2},
		{SourceID: 3, OutputID: 3},
		{SourceID: 4, OutputID: 4},
	}
}

func safeWriteWork(work, name string, data []byte) (string, error) {
	path, err := pipeline.SafeJoin(work, name)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", model.Wrap(model.CodeInternal, model.StageFixture, "write "+name, err)
	}
	return path, nil
}

func probeCorruptBox(path string, expected int64) (int64, int64, int64, error) {
	_, err := bmff.ScanFile(path)
	me, ok := model.AsError(err)
	if err == nil || !ok || me.Code != model.CodeCorruptBox {
		return 0, 0, 0, model.Newf(model.CodeInternal, model.StageCLI, "expected corrupt box at %d, got %v", expected, err)
	}
	delta := me.DetectedOffset - me.ExpectedOffset
	if me.ExpectedOffset != expected || me.DetectedOffset != expected || delta != 0 {
		return me.ExpectedOffset, me.DetectedOffset, delta,
			model.Newf(model.CodeIntegrity, model.StageScan, "corrupt offset delta=%d", delta)
	}
	return me.ExpectedOffset, me.DetectedOffset, delta, nil
}

func appendUncommittedTail(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return model.Wrap(model.CodeInternal, model.StageFixture, "append demo tail", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("UNCOMMITTED_TAIL")); err != nil {
		return model.Wrap(model.CodeInternal, model.StageFixture, "write demo tail", err)
	}
	return nil
}
