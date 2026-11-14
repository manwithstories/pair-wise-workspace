package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"rcx/internal/fixture"
	"rcx/internal/job"
	"rcx/internal/rc"
	"rcx/internal/report"
)

func cmdEncode(args []string) int {
	fs := flag.NewFlagSet("encode", flag.ContinueOnError)
	planPath := fs.String("plan", "", "plan.json 路径")
	workers := fs.Int("workers", 4, "并发段数")
	checkpoint := fs.String("checkpoint", "", "检查点文件路径")
	out := fs.String("out", "", "输出目录（写 run.json）")
	if err := fs.Parse(args); err != nil {
		return ExitInvalid
	}
	if *planPath == "" || *checkpoint == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "缺少必填参数 --plan/--checkpoint/--out")
		return ExitInvalid
	}
	data, err := os.ReadFile(*planPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取计划失败:", err)
		return ExitInvalid
	}
	var plan rc.Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		fmt.Fprintln(os.Stderr, "计划文件非法:", err)
		return ExitInvalid
	}
	raw, err := os.ReadFile(plan.Input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取输入失败:", err)
		return ExitInvalid
	}
	frames, err := fixture.ReadFrames(raw, plan.Width, plan.Height)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析输入失败:", err)
		return ExitInvalid
	}
	if len(frames) != plan.Frames {
		fmt.Fprintf(os.Stderr, "输入帧数 %d 与计划 %d 不一致\n", len(frames), plan.Frames)
		return ExitInvalid
	}
	for _, dir := range []string{filepath.Dir(*checkpoint), *out} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "创建目录失败:", err)
			return ExitInvalid
		}
	}
	eventsLog := *checkpoint + ".events.log"
	machine := job.NewMachine(eventsLog, job.StatePending)
	for _, st := range []job.State{job.StatePlanning, job.StateEncoding} {
		if err := machine.Transition(st); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return ExitInvalid
		}
	}
	sched := &job.Scheduler{
		Workers: *workers, JournalPath: *checkpoint, MaxSegRetry: 2,
		Machine: machine, Controller: rc.NewController(), Plan: &plan, Frames: frames,
		OnSegmentDone: func(cp job.SegCheckpoint) {
			fmt.Printf("段 %d 完成：qp=%.2f bits=%d psnr=%.2fdB vbv_min=%.3f\n",
				cp.Index, cp.QP, cp.Bits, cp.PSNRMeanDB, cp.VBVMinRatio)
		},
	}
	j, err := sched.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "任务失败（检查点已保留，可续跑）:", err)
		machine.Transition(job.StateFailed)
		return ExitFailed
	}
	if err := machine.Transition(job.StateVerifying); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitInvalid
	}
	rep := report.Build(&plan, j)
	report.PrintTable(os.Stdout, rep, nil)
	if err := report.SaveJSON(filepath.Join(*out, "run.json"), rep); err != nil {
		fmt.Fprintln(os.Stderr, "写报告失败:", err)
		return ExitFailed
	}
	if err := machine.Transition(job.StateDone); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitInvalid
	}
	if rep.Verdict != "PASS" {
		return ExitFailed
	}
	return ExitOK
}
