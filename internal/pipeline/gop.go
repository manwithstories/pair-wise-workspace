package pipeline

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"turncode/internal/codec"
	"turncode/internal/config"
	"turncode/internal/quality"
	"turncode/internal/state"
	"turncode/internal/yuv"
)

func (x *execution) loop() (Outcome, error) {
	start := x.cp.NextFrame
	out := Outcome{ResumedFromGOP: x.cp.ResumedFromGOP}
	for x.cp.NextFrame < x.spec.Frames {
		if err := x.ctx.Err(); err != nil {
			return x.interrupt(out, true)
		}
		n := x.spec.GOP
		if remaining := x.spec.Frames - x.cp.NextFrame; remaining < n {
			n = remaining
		}
		frames, err := x.readFrames(n)
		if err != nil {
			x.markFailed()
			return out, err
		}
		activity := frameActivity(frames)
		sceneCut := x.prevFrame != nil && isHardCut(x.prevFrame, frames[0])
		cp, bytes, err := x.encodeAndCommit(start, frames, activity, sceneCut)
		if err != nil {
			x.markFailed()
			return out, err
		}
		x.cp = cp
		start = cp.NextFrame
		out.CompletedGOPs = cp.CompletedGOPs
		out.ResumedFromGOP = cp.ResumedFromGOP
		out.BitstreamBytes = cp.BitstreamOffset
		out.Checkpoint = cp
		x.prevFrame = frames[len(frames)-1]
		_ = bytes
		if x.cp.NextFrame >= x.spec.Frames {
			break
		}
		if x.job.FailAfterGOP > 0 && x.cp.CompletedGOPs >= x.job.FailAfterGOP {
			return x.interrupt(out, false)
		}
		if x.job.CancelAfterGOP > 0 && x.cp.CompletedGOPs >= x.job.CancelAfterGOP {
			return x.interrupt(out, true)
		}
		if err := x.ctx.Err(); err != nil {
			return x.interrupt(out, true)
		}
	}
	if x.cp.NextFrame != x.spec.Frames {
		x.markFailed()
		return out, fmt.Errorf("pipeline: frame accounting mismatch: next=%d frames=%d", x.cp.NextFrame, x.spec.Frames)
	}
	if err := x.machine.Transition(state.Verifying); err != nil {
		return out, err
	}
	if _, err := x.states.Save(state.Verifying); err != nil {
		return out, err
	}
	info, err := x.bitstream.Stat()
	if err != nil {
		x.markFailed()
		return out, err
	}
	if info.Size() != x.cp.BitstreamOffset {
		x.markFailed()
		return out, fmt.Errorf("pipeline: bitstream/checkpoint size mismatch")
	}
	if err := x.machine.Transition(state.Done); err != nil {
		return out, err
	}
	if _, err := x.states.Save(state.Done); err != nil {
		return out, err
	}
	out.Completed = true
	out.Interrupted = false
	out.CompletedGOPs = x.cp.CompletedGOPs
	out.BitstreamBytes = info.Size()
	out.Checkpoint = x.cp
	return out, nil
}

func (x *execution) interrupt(out Outcome, cancelled bool) (Outcome, error) {
	out.Interrupted = true
	out.Cancelled = cancelled
	out.CompletedGOPs = x.cp.CompletedGOPs
	out.BitstreamBytes = x.cp.BitstreamOffset
	out.Checkpoint = x.cp
	if err := x.machine.Transition(state.Paused); err != nil {
		return out, fmt.Errorf("pipeline: pause: %w", err)
	}
	if _, err := x.states.Save(state.Paused); err != nil {
		return out, err
	}
	return out, &InterruptedError{CompletedGOPs: x.cp.CompletedGOPs, Cancelled: cancelled}
}

func (x *execution) markFailed() {
	if x.machine.State() == state.Encoding {
		_ = x.machine.Transition(state.Failed)
		_, _ = x.states.Save(state.Failed)
	}
}

func (x *execution) readFrames(n int) ([]*yuv.Frame, error) {
	frames := make([]*yuv.Frame, 0, n)
	for i := 0; i < n; i++ {
		f, err := yuv.ReadFrame(x.source, x.spec.Width, x.spec.Height)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("pipeline: source ended early at frame %d", x.cp.NextFrame+i)
			}
			return nil, err
		}
		frames = append(frames, f)
	}
	return frames, nil
}

func (x *execution) encodeAndCommit(start int, frames []*yuv.Frame, activity float64, sceneCut bool) (state.Checkpoint, int64, error) {
	sel, err := x.selectAndEncode(frames, activity, sceneCut)
	if err != nil {
		return x.cp, 0, err
	}
	psnrs, err := decodeQuality(sel.payloads, frames, x.spec.Width, x.spec.Height)
	if err != nil {
		return x.cp, 0, err
	}
	for i, psnr := range psnrs {
		x.cp.Quality.Add(start+i, psnr)
	}
	for _, payload := range sel.payloads {
		if err := codec.WriteRecord(x.bitstream, payload); err != nil {
			return x.cp, 0, err
		}
	}
	if err := x.bitstream.Sync(); err != nil {
		return x.cp, 0, err
	}
	offset, err := x.bitstream.Seek(0, io.SeekCurrent)
	if err != nil {
		return x.cp, 0, err
	}
	x.controller.Observe(sel.rateQP, sel.rateBits, len(frames), activity)
	x.cp.Version = state.JournalVersion
	x.cp.SpecHash = x.meta.SpecHash
	x.cp.CompletedGOPs++
	x.cp.NextFrame = start + len(frames)
	x.cp.BitstreamOffset = offset
	x.cp.QP = sel.actualQP
	x.cp.Buffer = x.controller.Snapshot()
	x.cp.LastGOPBytes = sel.actualBytes
	x.cp.LastGOPFrames = len(frames)
	if err := x.checkpoints.SaveCheckpoint(x.cp); err != nil {
		return x.cp, 0, err
	}
	return x.cp, sel.actualBytes, nil
}

type gopSelection struct {
	actualQP    int
	qps         []int
	payloads    [][]byte
	actualBytes int64
	rateQP      int
	rateBits    int64
}

const qpOffsetScale = 4

func (x *execution) selectAndEncode(frames []*yuv.Frame, activity float64, sceneCut bool) (gopSelection, error) {
	sel, err := x.selectRateTarget(frames, activity, sceneCut)
	if err != nil || x.job.QPOffset == 0 {
		return sel, err
	}
	offsetQPs := make([]int, len(sel.qps))
	var weightedQP int
	for i, qp := range sel.qps {
		offsetQPs[i] = clampQP(qp + x.job.QPOffset*qpOffsetScale)
		weightedQP += offsetQPs[i]
	}
	payloads, bytes, err := encodeTrialQPs(frames, offsetQPs)
	if err != nil {
		return gopSelection{}, err
	}
	sel.actualQP = clampQP(int(math.Round(float64(weightedQP) / float64(len(offsetQPs)))))
	sel.qps = offsetQPs
	sel.payloads = payloads
	sel.actualBytes = bytes
	return sel, nil
}

func (x *execution) selectRateTarget(frames []*yuv.Frame, activity float64, sceneCut bool) (gopSelection, error) {
	base := x.controller.QPForGOP(len(frames), activity)
	if sceneCut {
		x.controller.ResetScene(base)
	}
	target := x.controller.BudgetBits(len(frames))
	debug := os.Getenv("TURNCODE_DEBUG") != ""
	qp := base
	var bestPayloads [][]byte
	var bestBytes int64
	bestDeviation := math.Inf(1)
	bestQP := qp
	var underTrial, overTrial encodedTrial
	for trial := 0; trial < 4; trial++ {
		payloads, bytes, err := encodeTrial(x.spec, frames, qp)
		if err != nil {
			return gopSelection{}, err
		}
		deviation := math.Abs(float64(bytes)*8 - target)
		if debug {
			fmt.Fprintf(os.Stderr, "gop=%d trial qp=%d bits=%d target=%.0f dev=%.0f\n", x.cp.CompletedGOPs, qp, bytes*8, target, deviation)
		}
		if deviation < bestDeviation {
			bestDeviation = deviation
			bestPayloads, bestBytes, bestQP = payloads, bytes, qp
		}
		if float64(bytes)*8 >= target {
			if overTrial.bytes == 0 || bytes < overTrial.bytes {
				overTrial = encodedTrial{qp: qp, payloads: payloads, bytes: bytes}
			}
		} else if underTrial.bytes == 0 || bytes > underTrial.bytes {
			underTrial = encodedTrial{qp: qp, payloads: payloads, bytes: bytes}
		}
		tolerance := math.Max(128, target*0.015)
		if deviation <= tolerance {
			break
		}
		next := x.controller.AdjustQP(target, float64(bytes)*8, qp)
		if next == qp {
			break
		}
		qp = next
	}
	if bestPayloads == nil {
		return gopSelection{}, fmt.Errorf("pipeline: GOP encode produced no payloads")
	}
	if bestDeviation > math.Max(128, target*0.015) && underTrial.bytes > 0 && overTrial.bytes > 0 {
		mixedQPs, mixedPayloads, mixedBytes, err := mixQP(frames, target, underTrial, overTrial)
		if err != nil {
			return gopSelection{}, err
		}
		if mixedPayloads != nil {
			mixedQP := averageQP(mixedQPs)
			if debug {
				fmt.Fprintf(os.Stderr, "gop=%d mixed qp=%d bits=%d target=%.0f\n", x.cp.CompletedGOPs, mixedQP, mixedBytes*8, target)
			}
			return gopSelection{actualQP: mixedQP, qps: mixedQPs, payloads: mixedPayloads, actualBytes: mixedBytes, rateQP: mixedQP, rateBits: mixedBytes * 8}, nil
		}
	}
	qps := make([]int, len(frames))
	for i := range qps {
		qps[i] = bestQP
	}
	return gopSelection{actualQP: bestQP, qps: qps, payloads: bestPayloads, actualBytes: bestBytes, rateQP: bestQP, rateBits: bestBytes * 8}, nil
}

type encodedTrial struct {
	qp       int
	payloads [][]byte
	bytes    int64
}

// mixQP fills the gap between two integer-QP rates by assigning adjacent QPs to
// individual frames. It keeps the GOP budget accurate without fractional QPs.
func mixQP(frames []*yuv.Frame, target float64, under, over encodedTrial) ([]int, [][]byte, int64, error) {
	bitsUnder := float64(under.bytes) * 8
	bitsOver := float64(over.bytes) * 8
	if bitsOver <= bitsUnder || target <= bitsUnder || target >= bitsOver {
		return nil, nil, 0, nil
	}
	n := len(frames)
	highCount := int(math.Round(float64(n) * (target - bitsUnder) / (bitsOver - bitsUnder)))
	if highCount < 0 {
		highCount = 0
	} else if highCount > n {
		highCount = n
	}
	bestDeviation := math.Inf(1)
	var bestPayloads [][]byte
	var bestBytes int64
	var bestCount int
	for iter := 0; iter < 3 && highCount >= 0 && highCount <= n; iter++ {
		qps := mixedQPList(n, highCount, over.qp, under.qp)
		payloads, bytes, err := encodeTrialQPs(frames, qps)
		if err != nil {
			return nil, nil, 0, err
		}
		deviation := math.Abs(float64(bytes)*8 - target)
		if deviation < bestDeviation {
			bestDeviation, bestPayloads, bestBytes, bestCount = deviation, payloads, bytes, highCount
		}
		if deviation <= math.Max(128, target*0.012) || bitsOver-bitsUnder == 0 {
			break
		}
		estimated := float64(highCount) * (target - float64(bytes)*8) / (bitsOver - bitsUnder)
		next := int(math.Round(float64(highCount) + estimated))
		if next < 0 {
			next = 0
		} else if next > n {
			next = n
		}
		if next == highCount {
			break
		}
		highCount = next
	}
	if bestPayloads == nil {
		return nil, nil, 0, nil
	}
	return mixedQPList(n, bestCount, over.qp, under.qp), bestPayloads, bestBytes, nil
}

func averageQP(qps []int) int {
	if len(qps) == 0 {
		return 0
	}
	sum := 0
	for _, qp := range qps {
		sum += qp
	}
	return clampQP(int(math.Round(float64(sum) / float64(len(qps)))))
}

func mixedQPList(n, highCount, highQP, lowQP int) []int {
	qps := make([]int, n)
	for i := range qps {
		if highCount > 0 && (i*highCount)%n < highCount {
			qps[i] = highQP
		} else {
			qps[i] = lowQP
		}
	}
	return qps
}

func encodeTrial(spec config.Spec, frames []*yuv.Frame, qp int) ([][]byte, int64, error) {
	qps := make([]int, len(frames))
	for i := range qps {
		qps[i] = qp
	}
	return encodeTrialQPs(frames, qps)
}

func encodeTrialQPs(frames []*yuv.Frame, qps []int) ([][]byte, int64, error) {
	if len(frames) != len(qps) {
		return nil, 0, fmt.Errorf("pipeline: QP/frame count mismatch")
	}
	enc, err := codec.NewEncoder(frames[0].Width, frames[0].Height)
	if err != nil {
		return nil, 0, err
	}
	payloads := make([][]byte, 0, len(frames))
	var bytes int64
	for i, frame := range frames {
		payload, _, err := enc.EncodeFrame(frame, i > 0, qps[i])
		if err != nil {
			return nil, 0, err
		}
		payloads = append(payloads, payload)
		bytes += int64(len(payload)) + 4
	}
	return payloads, bytes, nil
}

func decodeQuality(payloads [][]byte, frames []*yuv.Frame, width, height int) ([]float64, error) {
	var ref *yuv.Frame
	out := make([]float64, len(payloads))
	for i, payload := range payloads {
		rec, _, _, err := codec.DecodeFrame(payload, width, height, ref)
		if err != nil {
			return nil, fmt.Errorf("pipeline: decode GOP frame %d: %w", i, err)
		}
		psnr, err := quality.FramePSNR(frames[i], rec)
		if err != nil {
			return nil, err
		}
		out[i] = psnr
		ref = rec
	}
	return out, nil
}

func frameActivity(frames []*yuv.Frame) float64 {
	if len(frames) == 0 {
		return 0
	}
	var sum float64
	var count int
	for _, f := range frames {
		for y := 0; y < f.Height; y++ {
			row := y * f.Width
			for x := 1; x < f.Width; x++ {
				d := int(f.Y[row+x]) - int(f.Y[row+x-1])
				if d < 0 {
					d = -d
				}
				sum += float64(d)
				count++
			}
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count) / 64
}

func isHardCut(a, b *yuv.Frame) bool {
	if a == nil || b == nil || a.Width != b.Width || a.Height != b.Height {
		return false
	}
	var sum float64
	for i := range a.Y {
		d := int(a.Y[i]) - int(b.Y[i])
		if d < 0 {
			d = -d
		}
		sum += float64(d)
	}
	return sum/float64(len(a.Y)) > 12
}

func clampQP(qp int) int {
	if qp < 1 {
		return 1
	}
	if qp > 51 {
		return 51
	}
	return qp
}
