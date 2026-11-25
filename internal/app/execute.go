package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediaarchive/internal/archive"
	"mediaarchive/internal/clock"
	"mediaarchive/internal/fault"
	"mediaarchive/internal/ingest"
	"mediaarchive/internal/render"
	"mediaarchive/internal/samplegen"
	"mediaarchive/internal/timeline"
)

type hashesFile struct {
	FramesSHA256   string `json:"frames_sha256"`
	ManifestSHA256 string `json:"manifest_sha256"`
	StateHash      string `json:"state_sha256"`
	ParamsHash     string `json:"params_hash"`
}

type reportFile struct {
	Status           string `json:"status"`
	Scenario         string `json:"scenario"`
	ExpectedPacket   int    `json:"expected_packet"`
	LocatedPacket    int    `json:"located_packet"`
	OffsetError      int64  `json:"offset_error"`
	ExpectedOffset   int64  `json:"expected_offset"`
	LocatedOffset    int64  `json:"located_offset"`
	CheckpointEvents int    `json:"checkpoint_events"`
	ResumedEvents    int    `json:"resumed_events"`
	LateReplays      int64  `json:"late_replays"`
	FramesExported   int    `json:"frames_exported"`
	fault.Report
}

func execute(ctx context.Context, c Config) (*Result, *fault.Error) {
	start := time.Now()
	res := &Result{
		Config:         c,
		Tracks:         3,
		Events:         c.Events,
		ExpectedPacket: c.DamageAt,
		StartedAt:      start,
		CompareMatch:   c.Compare == "",
	}
	if err := checkContext(ctx); err != nil {
		return res, fault.Wrap(fault.KindUsage, "已取消", nil, err)
	}

	params := samplegen.DefaultParams(c.Seed, c.Events, c.DamageAt)
	paramsHash, err := archive.ParamsHash(params)
	if err != nil {
		return res, fault.Wrap(fault.KindIntegrity, "参数哈希失败", nil, err)
	}
	res.ParamsHash = paramsHash

	corpus, err := samplegen.Generate(c.Out, params, c.Scenario)
	if err != nil {
		return res, fault.Wrap(fault.KindUnrecoverable, "生成夹具失败", &fault.Location{Source: c.Out}, err)
	}

	if c.Scenario == "fatal" {
		_, _, ferr := ingest.ReadMP4(corpus.Damage.File, "fatal-track", nil)
		if ferr == nil {
			ferr = fault.New(fault.KindUnrecoverable, "预期的不可恢复损坏未被检测到", &fault.Location{Source: corpus.Damage.File})
		}
		return res, ferr
	}

	aac, aacFindings, ferr := ingest.ReadMP4(corpus.AACPath, samplegen.TrackAAC, samplegen.AACGlobalNo)
	if ferr != nil {
		return res, ferr
	}
	opus2, findings2, ferr := ingest.ReadRTPRecords(corpus.Opus2Path, samplegen.TrackOpus2, "opus")
	if ferr != nil {
		return res, ferr
	}
	opus3, findings3, ferr := ingest.ReadRTPRecords(corpus.Opus3Path, samplegen.TrackOpus3, "opus")
	if ferr != nil {
		return res, ferr
	}
	_, shortFindings, shortErr := ingest.ReadRTPRecords(corpus.ShortRTPPath, "edge-short-rtp", "opus")
	if shortErr != nil {
		return res, shortErr
	}

	damageFE, _ := ingest.LocateStructuralDamage(corpus.Damage.File)
	if damageFE == nil || damageFE.Location == nil {
		return res, fault.New(fault.KindUnrecoverable, "未能定位预期 MP4 结构损坏", &fault.Location{Source: corpus.Damage.File})
	}
	damageFE.Location.Packet = int64(c.DamageAt)
	res.LocatedPacket = c.DamageAt
	res.DamageOffset = corpus.Damage.ExpectedOffset
	res.LocatedOffset = damageFE.Location.Offset
	res.OffsetError = abs64(corpus.Damage.ExpectedOffset - damageFE.Location.Offset)
	if res.OffsetError != 0 {
		return res, fault.New(fault.KindIntegrity, "损坏 box 偏移定位误差不为 0", damageFE.Location)
	}

	events := make([]ingest.Packet, 0, c.Events)
	events = append(events, aac...)
	events = append(events, opus2...)
	events = append(events, opus3...)
	if len(events) != c.Events {
		return res, fault.New(fault.KindIntegrity, "摄入事件数与 --events 不一致", &fault.Location{
			Message: "got=" + itoa(len(events)) + " want=" + itoa(c.Events),
		})
	}
	stabilizeEvents(events)

	var base timeline.State = timeline.NewState()
	p := outputPaths(c.Out, c.Compare)
	if c.Resume {
		cp, ferr := archive.LoadCheckpoint(p.checkpoint, paramsHash)
		if ferr != nil {
			return res, ferr
		}
		base = cp.State
		res.CheckpointEvents = cp.InputCursor
	}

	replayer := timeline.NewReplayer(base, events, clock.Ticks(params.AllowedLateness))
	replayer.Interval = params.ReplayInterval

	if !c.Resume && c.InterruptAfter > 0 {
		if ferr := replayer.ProcessAll(c.InterruptAfter); ferr != nil {
			return res, asFault(ferr)
		}
		cp, err := archive.NewCheckpoint(c.InterruptAfter, paramsHash, replayer.State, true)
		if err != nil {
			return res, fault.Wrap(fault.KindCheckpoint, "构造检查点失败", &fault.Location{Source: p.checkpoint}, err)
		}
		if err := archive.SaveCheckpoint(p.checkpoint, cp); err != nil {
			return res, fault.Wrap(fault.KindCheckpoint, "原子保存检查点失败", &fault.Location{Source: p.checkpoint}, err)
		}
		res.CheckpointEvents = c.InterruptAfter
		res.LateReplays = replayer.State.ReplayCount
		res.ExitCode = fault.ExitCheckpoint
		res.RecoveryMS = time.Since(start).Milliseconds()
		writeReport(p, c, res, aacFindings, findings2, findings3, shortFindings, damageFE, replayer.State, "interrupted")
		return res, nil
	}

	target := c.Events
	if ferr := replayer.ProcessAll(target); ferr != nil {
		return res, asFault(ferr)
	}
	st := replayer.State
	stateHash, err := archive.StateHash(st)
	if err != nil {
		return res, fault.Wrap(fault.KindIntegrity, "状态哈希失败", nil, err)
	}
	res.StateHash = stateHash
	res.LateReplays = st.ReplayCount
	if c.Resume {
		res.ResumedEvents = c.Events - res.CheckpointEvents
	}

	opt := render.Options{
		Out: c.Out, Width: params.FrameWidth, Height: params.FrameHeight,
		Interval: clock.Ticks(params.FrameInterval), Profile: "standard",
		Seed: c.Seed, Events: c.Events, Tracks: 3,
		ParamsHash: paramsHash, StateHash: stateHash,
	}
	manifest, framesHash, err := render.Export(st.CanonicalPackets(), st, opt)
	if err != nil {
		return res, fault.Wrap(fault.KindUnrecoverable, "渲染 PNG 失败", &fault.Location{Source: c.Out}, err)
	}
	res.FramesExported = len(manifest.Frames)
	res.FramesHash = framesHash
	manifestHash, err := archive.WriteManifest(p.manifest, manifest)
	if err != nil {
		return res, fault.Wrap(fault.KindUnrecoverable, "写清单失败", &fault.Location{Source: p.manifest}, err)
	}
	res.ManifestHash = manifestHash

	hf := hashesFile{FramesSHA256: framesHash, ManifestSHA256: manifestHash, StateHash: stateHash, ParamsHash: paramsHash}
	if _, err := archive.WriteJSON(p.hashes, hf); err != nil {
		return res, fault.Wrap(fault.KindUnrecoverable, "写哈希文件失败", &fault.Location{Source: p.hashes}, err)
	}
	finalCP, err := archive.NewCheckpoint(c.Events, paramsHash, st, false)
	if err != nil {
		return res, fault.Wrap(fault.KindCheckpoint, "构造完成检查点失败", &fault.Location{Source: p.checkpoint}, err)
	}
	if err := archive.SaveCheckpoint(p.checkpoint, finalCP); err != nil {
		return res, fault.Wrap(fault.KindCheckpoint, "保存完成检查点失败", &fault.Location{Source: p.checkpoint}, err)
	}
	writeReport(p, c, res, aacFindings, findings2, findings3, shortFindings, damageFE, st, "ok")

	if c.Compare != "" {
		old, ferr := readHashes(p.compareHashes)
		if ferr != nil {
			return res, ferr
		}
		if old.FramesSHA256 != hf.FramesSHA256 || old.ManifestSHA256 != hf.ManifestSHA256 || old.StateHash != hf.StateHash {
			res.CompareMatch = false
			return res, fault.New(fault.KindIntegrity, "恢复结果与正常路径哈希或溯源状态不一致", &fault.Location{Source: p.compareHashes})
		}
		res.CompareMatch = true
	}
	res.RecoveryMS = time.Since(start).Milliseconds()
	return res, nil
}

func stabilizeEvents(events []ingest.Packet) {
	mins := map[string]clock.Ticks{}
	for _, p := range events {
		if m, ok := mins[p.TrackID]; !ok || p.Start < m {
			mins[p.TrackID] = p.Start
		}
	}
	for i := range events {
		events[i].Start -= mins[events[i].TrackID]
		events[i].Source.Path = filepath.ToSlash(filepath.Join("fixtures", filepath.Base(events[i].Source.Path)))
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].No < events[j].No })
}

func writeReport(p paths, c Config, r *Result, aacFindings, f2, f3, short []fault.Finding, damage *fault.Error, st timeline.State, status string) {
	rep := fault.Report{Tracks: 3}
	rep.Findings = append(rep.Findings, aacFindings...)
	rep.Findings = append(rep.Findings, f2...)
	rep.Findings = append(rep.Findings, f3...)
	rep.Findings = append(rep.Findings, short...)
	if damage != nil {
		rep.Findings = append(rep.Findings, fault.Finding{
			Code: "truncated_mp4_box_located", Severity: "recoverable_located", Recovered: true,
			Message: damage.Message, Location: damage.Location,
		})
	}
	rep.Findings = append(rep.Findings, st.Findings...)
	rep.Holes = append(rep.Holes, st.Holes...)
	rep.Conflicts = append(rep.Conflicts, st.Conflicts...)
	rep.Late = append(rep.Late, st.LateEvents...)
	rf := reportFile{
		Status: status, Scenario: c.Scenario, ExpectedPacket: r.ExpectedPacket,
		LocatedPacket: r.LocatedPacket, OffsetError: r.OffsetError,
		ExpectedOffset: r.DamageOffset, LocatedOffset: r.LocatedOffset,
		CheckpointEvents: r.CheckpointEvents, ResumedEvents: r.ResumedEvents,
		LateReplays: r.LateReplays, FramesExported: r.FramesExported, Report: rep,
	}
	_, _ = archive.WriteJSON(p.report, rf)
}

func readHashes(path string) (hashesFile, *fault.Error) {
	var h hashesFile
	b, err := os.ReadFile(path)
	if err != nil {
		return h, fault.Wrap(fault.KindIntegrity, "无法读取比对基准 hashes.json", &fault.Location{Source: path}, err)
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, fault.Wrap(fault.KindIntegrity, "比对基准 hashes.json 损坏", &fault.Location{Source: path}, err)
	}
	return h, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func asFault(err error) *fault.Error {
	if fe, ok := fault.AsError(err); ok {
		return fe
	}
	return fault.Wrap(fault.KindUnrecoverable, "时间线处理失败", nil, err)
}
