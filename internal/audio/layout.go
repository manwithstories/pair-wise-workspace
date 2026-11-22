// Package audio 处理声道布局映射与采样率边界。
package audio

import (
	"fmt"

	"mediatool/internal/clock"
)

// Channel 是具名声道。
type Channel string

const (
	FL  Channel = "FL"
	FR  Channel = "FR"
	FC  Channel = "FC"
	LFE Channel = "LFE"
	SL  Channel = "SL"
	SR  Channel = "SR"
)

// Layout 描述一个声道布局：名称 + 有序声道列表。
type Layout struct {
	Name     string
	Channels []Channel
}

var layouts = map[string]Layout{
	"mono":   {"mono", []Channel{FC}},
	"stereo": {"stereo", []Channel{FL, FR}},
	"5.1":    {"5.1", []Channel{FL, FR, FC, LFE, SL, SR}},
}

// Lookup 按名称查找布局。
func Lookup(name string) (Layout, bool) {
	l, ok := layouts[name]
	return l, ok
}

// NumChannels 返回声道数。
func (l Layout) NumChannels() int { return len(l.Channels) }

// MapSamples 按显式映射表把 in 布局的交错采样重排为 out 布局。
// mapping[i] 表示输出第 i 声道取输入第 mapping[i] 声道。
// 布局不同而缺失映射时返回可定位故障——禁止静默重排声道。
func MapSamples(in []float32, inL, outL Layout, mapping []int) ([]float32, *clock.Fault) {
	inCh, outCh := inL.NumChannels(), outL.NumChannels()
	if inCh == 0 || outCh == 0 {
		return nil, &clock.Fault{Kind: clock.FaultChannelMapMissing,
			Offset: clock.NoPosition, PID: clock.NoPosition,
			Detail: "empty channel layout"}
	}
	if len(in)%inCh != 0 {
		return nil, &clock.Fault{Kind: clock.FaultChannelMapMissing,
			Offset: clock.NoPosition, PID: clock.NoPosition,
			Detail: fmt.Sprintf("sample count %d not divisible by %d channels", len(in), inCh)}
	}
	if len(mapping) == 0 {
		if inL.Name == outL.Name {
			out := make([]float32, len(in))
			copy(out, in)
			return out, nil
		}
		return nil, &clock.Fault{Kind: clock.FaultChannelMapMissing,
			Offset: clock.NoPosition, PID: clock.NoPosition,
			Detail: fmt.Sprintf("no channel mapping for %s -> %s", inL.Name, outL.Name)}
	}
	if len(mapping) != outCh {
		return nil, &clock.Fault{Kind: clock.FaultChannelMapMissing,
			Offset: clock.NoPosition, PID: clock.NoPosition,
			Detail: fmt.Sprintf("mapping has %d entries, want %d", len(mapping), outCh)}
	}
	frames := len(in) / inCh
	out := make([]float32, frames*outCh)
	for f := 0; f < frames; f++ {
		for oc := 0; oc < outCh; oc++ {
			ic := mapping[oc]
			if ic < 0 || ic >= inCh {
				return nil, &clock.Fault{Kind: clock.FaultChannelMapMissing,
					Offset: clock.NoPosition, PID: clock.NoPosition,
					Detail: fmt.Sprintf("mapping[%d]=%d out of range [0,%d)", oc, ic, inCh)}
			}
			out[f*outCh+oc] = in[f*inCh+ic]
		}
	}
	return out, nil
}
