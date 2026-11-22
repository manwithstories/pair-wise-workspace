package model

// NormalizeSnapshot 是音频内核归一化结果的跨层快照，
// 放在 model 以打破 core 与 report 之间的导入环（两者都只依赖 model）。
type NormalizeSnapshot struct {
	TrackID        uint32
	InRate         uint32
	OutRate        uint32
	SamplesIn      int64
	SamplesOut     int64
	FrameDelta     int64
	PeakDBFS       float64
	IntegratedLUFS float64
	SilentFrames   int
	ClippedSamples int
	ShortFrames    int
}

// PackSnapshot 是再封装结果的跨层快照。
type PackSnapshot struct {
	ActualKBPS int
	MediaBytes int64
	FreeBytes  int64
}

// ColorSnapshot 是色彩仲裁的跨层快照。
type ColorSnapshot struct {
	Selected  ColorMeta
	Conflicts int
}
