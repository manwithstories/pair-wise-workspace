package rtsp

import "fmt"

// Capture error kinds mapped to distinct process exit codes by main.
const (
	ExitOK             = 0
	ExitArgs           = 2
	ExitVerifyMismatch = 3
	ExitHandshake      = 4
	ExitNonLoopback    = 5
)

// HandshakeError wraps an RTSP-level failure (bad status, broken connection).
type HandshakeError struct {
	Stage  string
	Status int
	Detail string
}

func (e *HandshakeError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("handshake failed at %s: status %d: %s", e.Stage, e.Status, e.Detail)
	}
	return fmt.Sprintf("handshake failed at %s: %s", e.Stage, e.Detail)
}

// AuthError is a persistent 401: the server rejected valid credentials.
type AuthError struct{ Stage string }

func (e *AuthError) Error() string {
	return "authentication failed (401) at " + e.Stage
}

// TransportError wraps a data-plane failure (cut, socket error). Read
// timeouts are represented separately by ReadTimeout.
type TransportError struct{ Detail string }

func (e *TransportError) Error() string { return "transport failure: " + e.Detail }
