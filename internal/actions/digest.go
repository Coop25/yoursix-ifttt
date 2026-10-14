package actions

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"net/http"
	"strings"
)

// HTTP Digest authentication (RFC 7616) - the default for Axis VAPIX and most
// other IP cameras. Only qop=auth is supported, which is what cameras offer.

type digestChallenge struct {
	realm, nonce, opaque, algorithm, qop string
}

func parseDigestChallenge(header string) (digestChallenge, bool) {
	scheme, params, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Digest") {
		return digestChallenge{}, false
	}
	var c digestChallenge
	for _, part := range splitParams(params) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "realm":
			c.realm = v
		case "nonce":
			c.nonce = v
		case "opaque":
			c.opaque = v
		case "algorithm":
			c.algorithm = v
		case "qop":
			c.qop = v
		}
	}
	return c, c.nonce != ""
}

// splitParams splits on commas that are not inside quotes.
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func digestAuthorization(c digestChallenge, method, uri, user, pass string) (string, error) {
	var newHash func() hash.Hash
	algo := strings.ToUpper(c.algorithm)
	switch algo {
	case "", "MD5":
		newHash = md5.New
	case "SHA-256":
		newHash = sha256.New
	default:
		return "", fmt.Errorf("unsupported digest algorithm %q", c.algorithm)
	}
	h := func(s string) string {
		x := newHash()
		x.Write([]byte(s))
		return hex.EncodeToString(x.Sum(nil))
	}

	ha1 := h(user + ":" + c.realm + ":" + pass)
	ha2 := h(method + ":" + uri)

	useQop := false
	for _, q := range strings.Split(c.qop, ",") {
		if strings.TrimSpace(q) == "auth" {
			useQop = true
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s"`, user, c.realm, c.nonce, uri)
	if useQop {
		cnonceBytes := make([]byte, 8)
		if _, err := rand.Read(cnonceBytes); err != nil {
			return "", err
		}
		cnonce := hex.EncodeToString(cnonceBytes)
		const nc = "00000001"
		resp := h(ha1 + ":" + c.nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
		fmt.Fprintf(&b, `, qop=auth, nc=%s, cnonce="%s", response="%s"`, nc, cnonce, resp)
	} else {
		fmt.Fprintf(&b, `, response="%s"`, h(ha1+":"+c.nonce+":"+ha2))
	}
	if c.opaque != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, c.opaque)
	}
	if c.algorithm != "" {
		fmt.Fprintf(&b, `, algorithm=%s`, c.algorithm)
	}
	return b.String(), nil
}

// pickDigestChallenge returns the strongest digest challenge from a 401.
func pickDigestChallenge(resp *http.Response) (digestChallenge, bool) {
	var best digestChallenge
	found := false
	for _, v := range resp.Header.Values("WWW-Authenticate") {
		c, ok := parseDigestChallenge(v)
		if !ok {
			continue
		}
		if !found || strings.EqualFold(c.algorithm, "SHA-256") {
			best, found = c, true
		}
	}
	return best, found
}
