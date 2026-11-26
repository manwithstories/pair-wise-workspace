// sources.go 实现五类 arc1 样例的具体生成与剪辑计划。
package fixture

import (
	"path/filepath"

	"archivemedia/internal/adapter"
	"archivemedia/internal/model"
)

// videoPackets 生成 n 帧视频包；duplicateFrom>0 时该帧起使用重复素材种子。
func videoPackets(n, duplicateFrom int) []adapter.ARCPacket {
	pkts := make([]adapter.ARCPacket, 0, n)
	for i := 0; i < n; i++ {
		dup := duplicateFrom > 0 && i >= duplicateFrom
		pkts = append(pkts, adapter.ARCPacket{
			TrackIndex:    0,
			StartTicks:    int64(i),
			DurationTicks: 1,
			KeyFrame:      i == 0,
			Payload:       gradientPayload(i, dup),
		})
	}
	return pkts
}

// genNormal：1s 双轨，含前 375ms 静音；两个剪辑点都落在帧网格上。
func genNormal(dir string) (Source, error) {
	pkts := append(videoPackets(25, 0), audioPackets(0, 8, 3)...)
	path := filepath.Join(dir, "normal.arc")
	if err := adapter.WriteARCFile(path,
		[]adapter.ARCTrackSpec{videoTrackSpec(0), audioTrackSpec()}, pkts, false); err != nil {
		return Source{}, err
	}
	return Source{
		Kind: KindNormal, Path: path, TargetBitrateBps: 0, // 实际码率即目标
		Plan: CutPlan{PlanRanges: []PlanRange{
			{0, frameUs(0), frameUs(10), model.RoundNearest, false},
			{0, frameUs(10), frameUs(20), model.RoundNearest, false},
			{1, 0, 500_000, model.RoundNearest, false},
		}},
	}, nil
}

// genTruncated：6 帧视频，最后一帧损坏；只恢复前 5 帧。
func genTruncated(dir string) (Source, error) {
	pkts := videoPackets(6, 0)
	path := filepath.Join(dir, "truncated.arc")
	if err := adapter.WriteARCFile(path,
		[]adapter.ARCTrackSpec{videoTrackSpec(0)}, pkts, true); err != nil {
		return Source{}, err
	}
	return Source{
		Kind: KindTruncated, Path: path, ExpectCorrupt: true,
		// 剪辑点限定在已验证前缀（前 5 帧 = 0..200ms）。
		Plan: CutPlan{PlanRanges: []PlanRange{
			{0, frameUs(0), frameUs(4), model.RoundNearest, false},
		}},
	}, nil
}

// genWrap：32 位回绕，刻度跨越 2^32 边界后继续。
func genWrap(dir string) (Source, error) {
	mod := int64(1) << 32
	raw := []int64{mod - 3, mod - 2, mod - 1, 0, 1, 2}
	pkts := make([]adapter.ARCPacket, 0, len(raw))
	for i, t := range raw {
		pkts = append(pkts, adapter.ARCPacket{
			TrackIndex: 0, StartTicks: t, DurationTicks: 1,
			KeyFrame: i == 0, Payload: gradientPayload(i, false),
		})
	}
	path := filepath.Join(dir, "wrap.arc")
	if err := adapter.WriteARCFile(path,
		[]adapter.ARCTrackSpec{videoTrackSpec(32)}, pkts, false); err != nil {
		return Source{}, err
	}
	return Source{
		Kind: KindWrap, Path: path, NoDurationLimit: true,
		// 回绕展开后的相对剪辑：用前两帧（mod-3..mod-1）。
		Plan: CutPlan{PlanRanges: []PlanRange{
			{0, frameUs(0), frameUs(2), model.RoundNearest, false},
		}},
	}, nil
}

// genIllegalCut：媒体正常，但剪辑计划引用越界时间与未知轨。
func genIllegalCut(dir string) (Source, error) {
	pkts := videoPackets(10, 0)
	path := filepath.Join(dir, "illegal.arc")
	if err := adapter.WriteARCFile(path,
		[]adapter.ARCTrackSpec{videoTrackSpec(0)}, pkts, false); err != nil {
		return Source{}, err
	}
	return Source{
		Kind: KindIllegalCut, Path: path,
		Plan: CutPlan{PlanRanges: []PlanRange{
			{0, frameUs(0), frameUs(5), model.RoundNearest, false}, // 合法对照
			{0, frameUs(0), 5_000_000, model.RoundNearest, true},    // 越界（素材仅 400ms）
			{7, 0, frameUs(2), model.RoundNearest, true},           // 未知轨
		}},
	}, nil
}

// genTransition：双轨视频重叠区间，转场被重复声明 3 次。
func genTransition(dir string) (Source, error) {
	trackB := videoTrackSpec(0)
	trackB.Codec = "arc-test-video-b"
	// 轨 0：10 帧；轨 1：10 帧，起始内容与轨 0 部分重复（种子 9）制造重复素材。
	pkts := videoPackets(10, 0)
	for i := 0; i < 10; i++ {
		pkts = append(pkts, adapter.ARCPacket{
			TrackIndex: 1, StartTicks: int64(i), DurationTicks: 1,
			KeyFrame: i == 0, Payload: gradientPayload(i, true),
		})
	}
	path := filepath.Join(dir, "transition.arc")
	if err := adapter.WriteARCFile(path,
		[]adapter.ARCTrackSpec{videoTrackSpec(0), trackB}, pkts, false); err != nil {
		return Source{}, err
	}
	return Source{
		Kind: KindTransition, Path: path,
		Plan: CutPlan{
			PlanRanges: []PlanRange{
				{0, frameUs(0), frameUs(6), model.RoundNearest, false},
			},
			Transitions: []PlanTransition{
				{0, 1, frameUs(2), frameUs(6), 3}, // 重复 3 次，去重为 1 段
			},
		},
	}, nil
}
