// Package adapter 定义版本化媒体解析契约与适配器注册表。
//
// 时间线及其余内核只依赖本包的 ParsedMedia / Parser 抽象，不直接接触
// go-astiav 或任何 CGO 类型，保证核心子系统可以在无 CGO 环境下单测。
// 具体适配器：
//   - arc1：纯 Go 的归档夹具容器（自生成样例、回绕时间戳、截断尾包）；
//   - astiav-v1：go-astiav 封装，解析 FFmpeg 支持的真实容器。
package adapter

import (
	"sort"
	"sync"

	"archivemedia/internal/model"
)

// MediaType 轨道介质类型。
type MediaType int

const (
	MediaUnknown MediaType = iota
	MediaVideo
	MediaAudio
)

// TrackKind 与 MediaType 的字符串映射在各适配器内复用。
func (m MediaType) String() string {
	switch m {
	case MediaVideo:
		return "video"
	case MediaAudio:
		return "audio"
	default:
		return "unknown"
	}
}

// Track 描述一条归一化前的轨道及其时基。
type Track struct {
	ID         string
	Index      int
	Type       MediaType
	TimeBase   model.Rational
	Width      int // 视频
	Height     int
	SampleRate int // 音频
	Channels   int
	Codec      string
	WrapBits   int // 时间戳回绕位宽，0 表示不回绕
}

// FramePacket 是一次解码粒度的时间戳记录。
//
// 对 arc1 夹具容器即原始帧/采样块；对 astiav 适配器是解封装得到的
// packet 元数据（内容指纹在适配器内按解码帧计算，见 Fingerprint 字段）。
type FramePacket struct {
	TrackIndex  int
	StartTicks  int64          // 轨道时基刻度
	DurationTicks int64
	KeyFrame    bool
	// ContentHash 是该 packet 解码后内容的确定性短哈希（如 xxHash），
	// 同内容不同封装应得到相同值；为空表示适配器未提供。
	ContentHash uint64
	// PayloadSize 用于码率核验，单位字节。
	PayloadSize int
}

// WrapEvent 标记一次时间戳回绕：在 AtTicks 处，刻度值从 High 跳回 Low。
type WrapEvent struct {
	TrackIndex int
	AtTicks    int64
	High       int64
	Low        int64
	WrapBits   int // 回绕位宽（如 33 位 MPEG-TS PCR）
}

// ParsedMedia 是适配器交给时间线内核的版本化解析结果。
type ParsedMedia struct {
	SourceID       string
	AdapterName    string // 如 "arc1" / "astiav"
	AdapterVersion string // 语义化版本，参与幂等键
	Container      string
	FormatVersion  string // 容器/编解码器版本，不兼容时由适配器拒绝
	Tracks         []Track
	Packets        []FramePacket
	Wraps          []WrapEvent
	// VerifiedPrefix 已成功验证的字节数；损坏尾包场景下小于文件总大小。
	VerifiedPrefix int64
	FileSize       int64
	// FirstBadAt 首个坏包对应的归一化前时间（适配器尽力换算）。
	FirstBadAt model.MediaTime
	// Truncated 为 true 表示尾包损坏但前缀可用。
	Truncated bool
}

// Capability 是适配器声明的能力位，供验收命令协商。
type Capability string

const (
	CapDemux      Capability = "demux"
	CapDecode     Capability = "decode"
	CapWrapExpand Capability = "wrap_expand"
	CapTruncation Capability = "truncation_prefix"
)

// Parser 是版本化解析器接口。
type Parser interface {
	Name() string
	Version() string
	Capabilities() []Capability
	// Probe 读取路径头部判断是否归属本适配器；不修改文件。
	Probe(path string) (bool, error)
	// Parse 执行解封装（及可选解码指纹），受 Limits 约束。
	Parse(path string, opts ParseOptions) (*ParsedMedia, error)
}

// ParseOptions 是资源限制与解析开关，所有字段必须有安全默认值。
type ParseOptions struct {
	MaxFileBytes  int64
	MaxTracks     int
	MaxDurationUs int64
	MaxConcurrency int
	ComputeHash   bool
}

// DefaultLimits 返回归档流水线的保守默认限制。
func DefaultLimits() ParseOptions {
	return ParseOptions{
		MaxFileBytes:   512 << 20, // 512 MiB
		MaxTracks:      16,
		MaxDurationUs:  2 * 60 * model.MicrosPerSecond,
		MaxConcurrency: 2,
		ComputeHash:    true,
	}
}

// ---- 注册表 ----

type registry struct {
	mu       sync.RWMutex
	parsers  map[string]Parser
	default_ string
}

var globalRegistry = &registry{parsers: map[string]Parser{}}

// Register 注册适配器；重复注册同名同版本返回错误以防静默覆盖。
func Register(p Parser) error {
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()
	key := p.Name() + "@" + p.Version()
	if _, ok := globalRegistry.parsers[key]; ok {
		return model.NewError(model.KindInvalidArgument, "adapter already registered: %s", key)
	}
	globalRegistry.parsers[key] = p
	if globalRegistry.default_ == "" {
		globalRegistry.default_ = key
	}
	return nil
}

// MustRegister 供 init 使用。
func MustRegister(p Parser) {
	if err := Register(p); err != nil {
		panic(err)
	}
}

// Get 按 name@version 精确取用。
func Get(name, version string) (Parser, bool) {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	p, ok := globalRegistry.parsers[name+"@"+version]
	return p, ok
}

// Latest 返回指定适配器名的最高语义版本。
func Latest(name string) (Parser, bool) {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	var best Parser
	for _, p := range globalRegistry.parsers {
		if p.Name() != name {
			continue
		}
		if best == nil || semverLess(best.Version(), p.Version()) {
			best = p
		}
	}
	return best, best != nil
}

// Negotiate 按 Probe 结果选择适配器，并要求所需能力全部满足。
func Negotiate(path string, need []Capability) (Parser, error) {
	globalRegistry.mu.RLock()
	all := make([]Parser, 0, len(globalRegistry.parsers))
	for _, p := range globalRegistry.parsers {
		all = append(all, p)
	}
	globalRegistry.mu.RUnlock()
	// 稳定顺序，保证协商结果可重复。
	sort.Slice(all, func(i, j int) bool {
		if all[i].Name() != all[j].Name() {
			return all[i].Name() < all[j].Name()
		}
		return semverLess(all[i].Version(), all[j].Version())
	})
	for _, p := range all {
		ok, err := p.Probe(path)
		if err != nil {
			return nil, err
		}
		if !ok || !hasCaps(p.Capabilities(), need) {
			continue
		}
		return p, nil
	}
	return nil, model.NewError(model.KindUnsupportedFormat,
		"no adapter can parse %s with caps %v", path, need)
}

// List 返回已注册适配器的描述（供终端输出与清单记录）。
type Info struct {
	Name    string
	Version string
	Caps    []Capability
}

// List 枚举注册表。
func List() []Info {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	out := make([]Info, 0, len(globalRegistry.parsers))
	for _, p := range globalRegistry.parsers {
		out = append(out, Info{Name: p.Name(), Version: p.Version(), Caps: append([]Capability(nil), p.Capabilities()...)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return semverLess(out[i].Version, out[j].Version)
	})
	return out
}

func hasCaps(have, need []Capability) bool {
	set := map[Capability]struct{}{}
	for _, c := range have {
		set[c] = struct{}{}
	}
	for _, c := range need {
		if _, ok := set[c]; !ok {
			return false
		}
	}
	return true
}

// semverLess 比较 "v1.2.3" / "1.2.3" 形态的语义版本，非数字段按字典序兜底。
func semverLess(a, b string) bool {
	pa := splitSemver(a)
	pb := splitSemver(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func splitSemver(v string) [3]int {
	out := [3]int{}
	cur, idx := 0, 0
	flush := func() {
		if idx < 3 {
			out[idx] = cur
		}
		idx++
		cur = 0
	}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			cur = cur*10 + int(r-'0')
		case r == '.':
			flush()
		default: // 'v'、'-' 预发布后缀等：数字部分忽略
			if r == 'v' && cur == 0 {
				continue
			}
			flush()
			return out
		}
	}
	flush()
	return out
}
