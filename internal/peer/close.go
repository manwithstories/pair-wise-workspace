package peer

// Close tears both peers down. It is idempotent and returns the first error
// encountered so a failed cleanup is visible rather than swallowed.
func (p *Pair) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	var first error
	if err := p.Sender.Close(); err != nil && first == nil {
		first = err
	}
	if err := p.Receiver.Close(); err != nil && first == nil {
		first = err
	}
	return first
}
