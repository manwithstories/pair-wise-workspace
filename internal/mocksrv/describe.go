package mocksrv

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
)

const authRealm = "mocksrv"

// handleDescribe answers DESCRIBE, enforcing one digest-auth round when
// credentials are configured.
func (s *Server) handleDescribe(req *base.Request, write func(*base.Response)) {
	if s.cfg.User != "" && !s.checkAuth(req) {
		write(&base.Response{
			StatusCode: base.StatusUnauthorized,
			Header: mergeCSeq(req, base.Header{
				"WWW-Authenticate": base.HeaderValue{fmt.Sprintf(
					`Digest realm="%s", nonce="%s", algorithm=MD5`, authRealm, s.nonce)},
			}),
		})
		return
	}
	res := ok(req, base.Header{"Content-Type": base.HeaderValue{"application/sdp"}})
	res.Body = []byte(s.sdpBody())
	write(res)
}

// checkAuth verifies the digest response against the configured user/pass
// and this server's nonce.
func (s *Server) checkAuth(req *base.Request) bool {
	v := req.Header["Authorization"]
	if len(v) == 0 {
		return false
	}
	p := parseDigestPairs(v[0])
	if p["nonce"] != s.nonce {
		return false
	}
	ha1 := md5hex(s.cfg.User + ":" + authRealm + ":" + s.cfg.Pass)
	ha2 := md5hex(string(req.Method) + ":" + p["uri"])
	want := md5hex(ha1 + ":" + s.nonce + ":" + ha2)
	return p["response"] == want
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// parseDigestPairs parses: Digest username="u", realm="r", ...
func parseDigestPairs(v string) map[string]string {
	out := make(map[string]string)
	v = strings.TrimSpace(strings.TrimPrefix(v, "Digest"))
	for _, part := range strings.Split(v, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.Trim(strings.TrimSpace(kv[1]), `"`)
	}
	return out
}

func mergeCSeq(req *base.Request, h base.Header) base.Header {
	for k, v := range cseqOf(req) {
		h[k] = v
	}
	return h
}

// sdpBody renders the session description. Track 0 advertises an absolute
// control URL, the remaining tracks relative ones, so the client must
// resolve both forms.
func (s *Server) sdpBody() string {
	base := fmt.Sprintf("rtsp://%s%s/", s.ln.Addr().String(), s.cfg.Path)
	var b strings.Builder
	b.WriteString("v=0\r\n")
	b.WriteString("o=- 1 1 IN IP4 127.0.0.1\r\n")
	b.WriteString("s=mocksrv\r\n")
	b.WriteString("c=IN IP4 127.0.0.1\r\n")
	b.WriteString("t=0 0\r\n")
	fmt.Fprintf(&b, "a=control:%s\r\n", base)
	for i := 0; i < s.cfg.Tracks; i++ {
		pt := 96 + i
		fmt.Fprintf(&b, "m=video 0 RTP/AVP %d\r\n", pt)
		fmt.Fprintf(&b, "a=rtpmap:%d H264/90000\r\n", pt)
		if i == 0 {
			fmt.Fprintf(&b, "a=control:%strackID=0\r\n", base)
		} else {
			fmt.Fprintf(&b, "a=control:trackID=%d\r\n", i)
		}
	}
	return b.String()
}
