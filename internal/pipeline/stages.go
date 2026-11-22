package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediapack/internal/cache"
	"mediapack/internal/codec"
	"mediapack/internal/fault"
	"mediapack/internal/mux"
	"mediapack/internal/rtpval"
	"mediapack/internal/track"
)

// drive runs the bounded producer and the RTP-validating fragment consumer in
// separate goroutines. Backpressure is a byte-sized counting semaphore: once
// the chunk budget of unconsumed bytes is in flight, the producer blocks.
func (p *Pipeline) drive(ctx context.Context, input []byte, resuming bool) (*Result, error) {
	groups := p.fragmentGroups()
	sem := make(chan struct{}, p.cfg.ChunkBytes)
	items := make(chan item, 4)
	errCh := make(chan error, 1)

	go func() {
		defer close(items)
		for _, au := range p.aus {
			n := au.PayloadLen()
			for k := 0; k < n; k++ {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					errCh <- fault.New(fault.KindCanceled, "pipeline", "producer canceled")
					return
				}
			}
			p.account(n)
			select {
			case items <- item{au: au, size: n}:
			case <-ctx.Done():
				errCh <- fault.New(fault.KindCanceled, "pipeline", "producer canceled")
				return
			}
		}
	}()

	go func() { errCh <- p.consume(ctx, input, groups, items, sem, resuming) }()

	if err := <-errCh; err != nil {
		return nil, err
	}
	return p.result(input, groups)
}

// consume validates one synthetic RTP packet per AU, groups AUs into
// fragments and commits each durable fragment to the checkpoint.
func (p *Pipeline) consume(ctx context.Context, input []byte, groups [][]codec.AccessUnit,
	items <-chan item, sem chan struct{}, resuming bool) error {
	validator := rtpval.NewValidator(1)
	groupIndex := 0
	var pending []codec.AccessUnit
	pendingBytes := 0

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		idx := groupIndex
		groupIndex++
		err := p.writeFragment(ctx, input, idx, pending, resuming)
		pending, pendingBytes = nil, 0
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return fault.New(fault.KindCanceled, "pipeline", "consumer canceled")
		case it, ok := <-items:
			if !ok {
				return flush()
			}
			if err := p.validatePacket(validator, it.au); err != nil {
				return err
			}
			n := it.size
			if len(pending) > 0 && pendingBytes+n > p.cfg.ChunkBytes {
				if err := flush(); err != nil {
					return err
				}
			}
			pending = append(pending, it.au)
			pendingBytes += n
			p.release(n)
			for k := 0; k < n; k++ {
				<-sem
			}
		}
	}
}

// validatePacket builds and checks one RTP packet carrying this AU.
func (p *Pipeline) validatePacket(v *rtpval.Validator, au codec.AccessUnit) error {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96,
			SequenceNumber: uint16(au.Seq),
			Timestamp:      uint32(au.PTS),
			SSRC:           0x4D504B31, // "MPK1"
			Marker:         au.Key,
		},
		Payload: au.NALs[0].Raw[:min(16, len(au.NALs[0].Raw))],
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return fault.Wrap(err, "rtp")
	}
	if _, _, err := v.Check(raw); err != nil {
		return err
	}
	p.packets.Add(1)
	return nil
}

// writeFragment produces one MP4 fragment, using the cache when possible and
// recording it in the checkpoint only after a durable fsynced write.
func (p *Pipeline) writeFragment(ctx context.Context, input []byte, idx int,
	aus []codec.AccessUnit, resuming bool) error {
	name := fragmentName(idx)
	st, _, err := p.cp.Load()
	if err != nil {
		return err
	}
	if st.SourceID != "" && containsFrag(st.CompletedFrags, name) && p.cp.FragmentExists(name) {
		// Already durable: never rewrite an existing fragment. Still
		// revalidate its content-addressed cache entry so a resume proves the
		// cached bytes really derive from this exact input.
		id := cache.Identity{
			SourceID:    p.cfg.SourceID + "/frag/" + name,
			InputSHA256: sha256Hex(input),
		}
		if _, hit, err := p.store.Get(id); err != nil {
			return err
		} else if hit {
			p.revalidated.Store(1)
		}
		return nil
	}
	if p.cfg.SlowConsumer > 0 && !resuming {
		select {
		case <-timeAfter(p.cfg.SlowConsumer):
		case <-ctx.Done():
			return fault.New(fault.KindCanceled, "pipeline", "slow consumer canceled")
		}
	}
	id := cache.Identity{
		SourceID:    p.cfg.SourceID + "/frag/" + name,
		InputSHA256: sha256Hex(input),
	}
	blob, hit, err := p.store.Get(id)
	if err != nil {
		return err
	}
	if !hit {
		var buf writeSeeker
		meta := &codec.StreamMeta{
			Codec: p.meta.Codec, TrackID: p.meta.TrackID,
			NumAU: len(aus), Keyframes: p.meta.Keyframes,
			Configs: p.meta.Configs,
		}
		if _, err := mux.Write(&buf, aus, meta); err != nil {
			return err
		}
		blob = buf.bytes()
		if _, err := p.store.Put(id, blob); err != nil {
			return err
		}
	} else {
		p.revalidated.Store(1)
	}
	if _, err := p.cp.FragmentDurable(name, blob); err != nil {
		return err
	}
	st.SourceID = p.cfg.SourceID
	st.InputSHA256 = sha256Hex(input)
	st.NextAU = idx + 1
	st.CompletedFrags = append(st.CompletedFrags, name)
	digest, err := p.outputDigest(input, st.CompletedFrags)
	if err != nil {
		return err
	}
	st.OutputSHA256 = digest // intermediate checkpoint value; result recomputes it
	if err := p.cp.Commit(st); err != nil {
		return err
	}
	if p.cfg.InterruptAt > 0 && idx+1 >= p.cfg.InterruptAt && !resuming {
		return &ErrInterrupted{Fragments: idx + 1}
	}
	return nil
}

func containsFrag(list []string, name string) bool {
	for _, f := range list {
		if f == name {
			return true
		}
	}
	return false
}

// result assembles the final report from the durable checkpoint.
func (p *Pipeline) result(input []byte, groups [][]codec.AccessUnit) (*Result, error) {
	st, ok, err := p.cp.Load()
	if err != nil {
		return nil, err
	}
	if !ok || len(st.CompletedFrags) != len(groups) {
		return nil, fault.New(fault.KindIO, "checkpoint", "incomplete fragments at finish")
	}
	finalDigest, err := p.outputDigest(input, st.CompletedFrags)
	if err != nil {
		return nil, err
	}
	tr := track.Map(p.meta.TrackID, track.Video, p.meta.Codec, p.aus, nil, nil)
	return &Result{
		Timeline: track.Summarize([]*track.Track{tr}),
		Metrics: Metrics{
			PeakBufferBytes:  p.peak,
			SourceSHA256:     sha256Hex(input),
			OutputSHA256:     finalDigest,
			CacheRevalidated: p.revalidated.Load() == 1,
			Packets:          int(p.packets.Load()),
			Fragments:        len(st.CompletedFrags),
			Keyframes:        len(p.meta.Keyframes),
		},
	}, nil
}

// outputDigest hashes every fragment file in the state directory in name
// order. It never depends on checkpoint history, so an interrupted-then-
// resumed run and a clean full run hash identically.
func (p *Pipeline) outputDigest(input []byte, frags []string) (string, error) {
	names := append([]string(nil), frags...)
	sortStrings(names)
	var b bytes.Buffer
	b.WriteString(p.cfg.SourceID)
	b.WriteString(sha256Hex(input))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(p.cp.Dir(), name))
		if err != nil {
			return "", fault.Wrap(err, "checkpoint")
		}
		b.WriteString(name)
		b.WriteString(sha256Hex(data))
	}
	return sha256Hex(b.Bytes()), nil
}

// sortStrings is a tiny insertion sort: fragment counts are small and this
// avoids pulling sort into the hot path.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
