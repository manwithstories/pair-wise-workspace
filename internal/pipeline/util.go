package pipeline

import (
	"encoding/json"
	"fmt"
	"mediad/internal/fixture"
	"mediad/internal/model"
	"mediad/internal/normalize"
	"mediad/internal/probe"
	"mediad/internal/txn"
)

func hasKind(list []fixture.FaultKind, k fixture.FaultKind) bool {
	for _, v := range list {
		if v == k {
			return true
		}
	}
	return false
}

func has(m map[txn.Stage]txn.Checkpoint, s txn.Stage) bool {
	_, ok := m[s]
	return ok
}

func mustJSON(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func validStage(s txn.Stage) bool { return stageIndex(s) >= 0 }

func stageIndex(s txn.Stage) int {
	for i, v := range txn.Order {
		if v == s {
			return i
		}
	}
	return -1
}

func probeAll(mp4Path, mxfPath string, out *model.Container, res *Result) error {
	cm, err := probe.MP4(mp4Path, probe.DefaultLimits())
	if err != nil {
		return err
	}
	cx, err := probe.MXF(mxfPath)
	if err != nil {
		return err
	}
	out.Tracks = append(out.Tracks, cm.Tracks...)
	out.Tracks = append(out.Tracks, cx.Tracks...)
	out.Format = "mxf+mp4"
	results, err := normalize.Container(out)
	if err != nil {
		return err
	}
	for _, n := range results {
		for _, note := range n.Notes {
			res.Notes = append(res.Notes, fmt.Sprintf("track %d: %s", n.TrackID, note))
		}
	}
	return nil
}

func lastCommitted(m map[txn.Stage]txn.Checkpoint) txn.Stage {
	var last txn.Stage
	for _, s := range txn.Order {
		if _, ok := m[s]; ok {
			last = s
		}
	}
	return last
}
