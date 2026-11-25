package transport

// BufferedPackets returns the number of RTP packets currently cached.
func (r *Receiver) BufferedPackets() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buffered
}

// Backpressure reports that the caller should pause socket reads until Poll
// releases cache entries.
func (r *Receiver) Backpressure() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buffered >= r.cfg.MaxPackets
}
