// Package adapter 定义媒体容器/传输适配器的版本化注册接口。
// 未知编解码器只能通过新版本适配器注册接入，统一内核不接受绕过注册的裸负载。
package adapter

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"mediacheck/internal/model"
)

// ParseInput 是一次解析请求：初始化段 + 有序分片字节负载（全部为内存安全上限内数据）。
type ParseInput struct {
	InitSegment []byte
	Fragments   [][]byte
	// MaxBoxSize / MaxInputBytes 由调用方按安全策略注入，适配器不得自行放大。
	MaxBoxSize    int64
	MaxInputBytes int64
}

// ParseResult 是适配器产出的统一内核视图。
type ParseResult struct {
	Tracks    []model.TrackMeta
	Fragments []model.Fragment
	Boxes     []model.BoxSummary // 初始化段 box 摘要（含未知 box）
}

// FragmentParser 是一个版本化的容器/传输解析器实现。
// Family 例如 "mp4"、"rtp"；Version 遵循 semver，注册表按 major 兼容选择。
type FragmentParser interface {
	Family() string
	Version() string
	Parse(ctx context.Context, in ParseInput) (*ParseResult, error)
}

type registryEntry struct {
	family     string
	major      int
	version    string
	parser     FragmentParser
	supported  map[string]bool // 该适配器声明支持的 codec 四字符码
}

// Registry 是进程内只读为主、带锁的版本化解析器注册表。
type Registry struct {
	mu      sync.RWMutex
	entries map[string][]registryEntry // key: family
	codec   map[string]string          // codec -> family
}

func NewRegistry() *Registry {
	return &Registry{entries: map[string][]registryEntry{}, codec: map[string]string{}}
}

// Register 注册一个适配器；同 family+major+version 重复注册直接拒绝，防止静默覆盖。
func (r *Registry) Register(p FragmentParser, codecs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	fam := p.Family()
	major, err := majorVersion(p.Version())
	if err != nil {
		return fmt.Errorf("adapter registry: %w", err)
	}
	for _, e := range r.entries[fam] {
		if e.version == p.Version() {
			return fmt.Errorf("adapter registry: %s %s already registered", fam, p.Version())
		}
	}
	sup := map[string]bool{}
	for _, c := range codecs {
		if c == "" {
			continue
		}
		if other, ok := r.codec[c]; ok && other != fam {
			return fmt.Errorf("adapter registry: codec %q claimed by %s and %s", c, other, fam)
		}
		r.codec[c] = fam
		sup[c] = true
	}
	r.entries[fam] = append(r.entries[fam], registryEntry{
		family: fam, major: major, version: p.Version(), parser: p, supported: sup,
	})
	sort.Slice(r.entries[fam], func(i, j int) bool { return r.entries[fam][i].major < r.entries[fam][j].major })
	return nil
}

// Lookup 按 family 选择不高于 wantMajor 的最高 major 适配器；wantMajor<=0 表示取最新。
func (r *Registry) Lookup(family string, wantMajor int) (FragmentParser, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.entries[family]
	if len(list) == 0 {
		return nil, &model.MediaError{Category: model.ErrUnknownCodec, Message: "no parser family: " + family}
	}
	var picked *registryEntry
	for i := range list {
		e := &list[i]
		if wantMajor > 0 && e.major > wantMajor {
			continue
		}
		if picked == nil || e.major > picked.major {
			picked = e
		}
	}
	if picked == nil {
		return nil, &model.MediaError{Category: model.ErrUnknownCodec,
			Message: fmt.Sprintf("no %s parser compatible with v%d", family, wantMajor)}
	}
	return picked.parser, nil
}

// RequireCodec 校验 codec 是否已被某适配器声明支持；未知 codec 必须走新版本适配器。
func (r *Registry) RequireCodec(codec string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.codec[codec]; !ok {
		return &model.MediaError{Category: model.ErrUnknownCodec, Message: "codec not registered: " + codec}
	}
	return nil
}

// Families 返回已注册 family 快照，用于报告与可复核输出。
func (r *Registry) Families() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for fam := range r.entries {
		out = append(out, fam)
	}
	sort.Strings(out)
	return out
}

func majorVersion(v string) (int, error) {
	if v == "" {
		return 0, fmt.Errorf("empty version")
	}
	n := 0
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		n = n*10 + int(v[i]-'0')
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("version %q must start with a number", v)
	}
	return n, nil
}
