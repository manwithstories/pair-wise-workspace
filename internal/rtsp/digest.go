package rtsp

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
)

// DigestAuthorization answers a 401 WWW-Authenticate digest challenge.
// It returns the value for the Authorization header of the single retry.
func DigestAuthorization(method, uri, user, pass, challenge string) (string, error) {
	params := parseDigestChallenge(challenge)
	realm, nonce := params["realm"], params["nonce"]
	if realm == "" || nonce == "" {
		return "", fmt.Errorf("rtsp: malformed digest challenge %q", challenge)
	}
	ha1 := md5Hex(user + ":" + realm + ":" + pass)
	ha2 := md5Hex(method + ":" + uri)
	resp := md5Hex(ha1 + ":" + nonce + ":" + ha2)
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
		user, realm, nonce, uri, resp), nil
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// parseDigestChallenge parses: Digest realm="r", nonce="n", ...
func parseDigestChallenge(s string) map[string]string {
	out := make(map[string]string)
	s = strings.TrimSpace(strings.TrimPrefix(s, "Digest"))
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.Trim(strings.TrimSpace(kv[1]), `"`)
	}
	return out
}
