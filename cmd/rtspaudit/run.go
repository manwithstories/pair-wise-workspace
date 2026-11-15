package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"rtspaudit/internal/mockserver"
	"rtspaudit/internal/report"
	auditrtp "rtspaudit/internal/rtp"
	auditrtsp "rtspaudit/internal/rtsp"
)

const (
	exitOK       = 0
	exitInternal = 1
	exitRTP      = 2
	exitProtocol = 3
	exitUsage    = 4
)

func (o *commonOptions) serverConfig() mockserver.Config {
	return mockserver.Config{
		Listen: "127.0.0.1:0", Frames: o.frames, Loss: o.loss,
		Reorder: o.reorder, Duplicate: o.duplicate, Seed: o.seed,
		Policy: o.policy, SessionTimeout: o.sessionTimeout, StallAfter: o.stallAfter,
	}
}

func executeOne(ctx context.Context, opts commonOptions, transport string) (auditrtsp.PullResult, error) {
	server, err := mockserver.Start(opts.serverConfig())
	if err != nil {
		return auditrtsp.PullResult{}, err
	}
	defer server.Close()
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return auditrtsp.Pull(runCtx, server.Addr(), auditrtsp.PullOptions{
		Transport: transport, SilenceTimeout: opts.silenceTimeout,
		MaxReconnects: opts.maxReconnects,
	})
}

func makePullReport(result auditrtsp.PullResult) report.PullReport {
	gaps := make([]auditrtp.Gap, 0, len(result.RTPSeqGaps))
	gaps = append(gaps, result.RTPSeqGaps...)
	hashes := append([]string(nil), result.Depacket.FrameHashes...)
	if hashes == nil {
		hashes = []string{}
	}
	return report.PullReport{
		Transport: result.Transport, StateTransitions: result.StateTransitions,
		IllegalTransitions: result.IllegalTransitions,
		DuplicatePackets:   result.DuplicatePackets, LateDropped: result.LateDropped,
		RTPSeqGaps: gaps, FramesReassembled: result.Depacket.FramesReassembled,
		DroppedFrames: result.Depacket.DroppedFrames, Keyframes: result.Depacket.Keyframes,
		Bytes: result.Depacket.Bytes, FrameHashes: hashes,
	}
}

func classifyError(err error, illegal int) int {
	if illegal > 0 || errors.Is(err, auditrtsp.ErrProtocol) ||
		errors.Is(err, auditrtsp.ErrSessionTimeout) ||
		errors.Is(err, auditrtsp.ErrSilenceExhausted) {
		return exitProtocol
	}
	if err != nil {
		return exitInternal
	}
	return exitOK
}

func runPull(ctx context.Context, opts commonOptions, transport string, stdout, stderr io.Writer) int {
	result, err := executeOne(ctx, opts, transport)
	if result.Transport == "" {
		fmt.Fprintln(stderr, "pull failed:", err)
		return exitInternal
	}
	rep := makePullReport(result)
	report.PrintPull(stdout, rep)
	if err != nil {
		fmt.Fprintln(stderr, "pull error:", err)
	}
	if writeErr := report.WriteJSON(opts.out, rep); writeErr != nil {
		fmt.Fprintln(stderr, "write report:", writeErr)
		return exitInternal
	}
	code := classifyError(err, rep.IllegalTransitions)
	if code == exitOK && (len(rep.RTPSeqGaps) > 0 || rep.DroppedFrames > 0 || rep.LateDropped > 0) {
		code = exitRTP
	}
	return code
}

func runCompare(ctx context.Context, opts commonOptions, stdout, stderr io.Writer) int {
	tcpResult, tcpErr := executeOne(ctx, opts, "tcp")
	udpResult, udpErr := executeOne(ctx, opts, "udp")
	if tcpResult.Transport == "" || udpResult.Transport == "" {
		fmt.Fprintln(stderr, "compare failed:", errors.Join(tcpErr, udpErr))
		return exitInternal
	}
	tcpReport, udpReport := makePullReport(tcpResult), makePullReport(udpResult)
	compare := report.NewCompare(tcpReport, udpReport)
	if writeErr := report.WriteJSON(opts.out, compare); writeErr != nil {
		fmt.Fprintln(stderr, "write report:", writeErr)
		return exitInternal
	}
	report.PrintCompare(stdout, compare)
	if tcpErr != nil {
		fmt.Fprintln(stderr, "tcp error:", tcpErr)
	}
	if udpErr != nil {
		fmt.Fprintln(stderr, "udp error:", udpErr)
	}
	tcpCode := classifyError(tcpErr, tcpReport.IllegalTransitions)
	udpCode := classifyError(udpErr, udpReport.IllegalTransitions)
	if tcpCode == exitProtocol || udpCode == exitProtocol {
		return exitProtocol
	}
	if tcpCode == exitInternal || udpCode == exitInternal {
		return exitInternal
	}
	if compare.Delta != 0 {
		return exitRTP
	}
	return exitOK
}
