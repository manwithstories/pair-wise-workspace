package fixture

import "media-audit/internal/sidecar"

// MetaLike 是 pipeline 依赖的帧元数据最小视图，解耦对 sidecar 具体类型的引用。
type MetaLike struct {
	Codec         string
	TrackID       int
	Timescale     uint32
	NominalStep   uint32
	KeyframeEvery int
	Frames        []sidecar.FrameMeta
	StreamSize    int64
	StreamSHA256  string
}

// AsMetaLike 把 sidecar 元数据转为 pipeline 使用的最小视图。
func AsMetaLike(m *sidecar.Meta) *MetaLike {
	return &MetaLike{
		Codec:         m.Codec,
		TrackID:       m.TrackID,
		Timescale:     m.Timescale,
		NominalStep:   m.NominalStep,
		KeyframeEvery: m.KeyframeEvery,
		Frames:        m.Frames,
		StreamSize:    m.StreamSize,
		StreamSHA256:  m.StreamSHA256,
	}
}
