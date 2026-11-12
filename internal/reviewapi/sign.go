package reviewapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Sign is the X-Pergent-Signature of a callback's body sent at t:
// t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">, with one v1= per
// secret, in order. During a rotation's overlap the current secret goes first
// and the previous one second, so a caller that checks either accepts it.
func Sign(body []byte, t time.Time, secrets ...string) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	var b strings.Builder
	b.WriteString("t=" + ts)
	for _, s := range secrets {
		m := hmac.New(sha256.New, []byte(s))
		m.Write([]byte(ts))
		m.Write([]byte("."))
		m.Write(body)
		b.WriteString(",v1=" + hex.EncodeToString(m.Sum(nil)))
	}
	return b.String()
}
