// Package lazy implements on-focus agent resume for Herdr sessions.
package lazy

import "strings"

// crockford is the Crockford base32 alphabet Herdr uses for public pane and
// tab numbers (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// EncodeNumber renders a public pane number the way Herdr prints it in ids
// such as "w8:p1E" (46) or "wE:p2B" (75).
func EncodeNumber(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [13]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = crockford[n%32]
		n /= 32
	}
	return string(buf[i:])
}

// DecodeNumber is the inverse of EncodeNumber. It reports false for anything
// EncodeNumber cannot produce.
func DecodeNumber(s string) (uint64, bool) {
	if s == "" || len(s) > 12 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		d := strings.IndexByte(crockford, s[i])
		if d < 0 {
			return 0, false
		}
		n = n*32 + uint64(d)
	}
	return n, true
}

// PaneID builds the public pane id for a workspace id and public pane number.
func PaneID(workspaceID string, publicNumber uint64) string {
	return workspaceID + ":p" + EncodeNumber(publicNumber)
}
