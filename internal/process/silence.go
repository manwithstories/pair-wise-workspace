package process

import "fmt"

// Interval 为时间区间，[Start, End) 主基准刻度。
type Interval struct {
	Start int64
	End   int64
}

// DetectSilence 对逐桶音量序列做滑动窗平均，并用滞回阈值判定静音：
// 窗均值低于 low 进入静音，高于 high 退出，介于两者之间保持原状态，
// 避免音量在阈值附近抖动造成区间碎片。bucketTicks 为每桶刻度数。
func DetectSilence(levels []float64, bucketTicks int64, low, high float64, window int) ([]Interval, error) {
	if bucketTicks <= 0 || window <= 0 {
		return nil, fmt.Errorf("bucketTicks and window must be positive")
	}
	if low >= high {
		return nil, fmt.Errorf("low threshold %f must be below high %f", low, high)
	}
	if len(levels) == 0 {
		return nil, nil
	}
	var out []Interval
	silent := false
	var runStart int64
	sum := 0.0
	for i, lv := range levels {
		sum += lv
		if i >= window {
			sum -= levels[i-window]
		}
		n := i + 1
		if n > window {
			n = window
		}
		avg := sum / float64(n)
		switch {
		case !silent && avg < low:
			silent = true
			runStart = int64(i) * bucketTicks
		case silent && avg > high:
			silent = false
			out = append(out, Interval{Start: runStart, End: int64(i) * bucketTicks})
		}
	}
	if silent {
		out = append(out, Interval{Start: runStart, End: int64(len(levels)) * bucketTicks})
	}
	return out, nil
}

// TotalTicks 汇总区间总刻度数。
func TotalTicks(iv []Interval) int64 {
	var t int64
	for _, v := range iv {
		t += v.End - v.Start
	}
	return t
}
