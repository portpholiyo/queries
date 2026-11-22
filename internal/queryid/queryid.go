// Package queryid generates and parses visitor-facing query IDs such as Q-7K3F9-XQ2MP.
// The ID is the only credential for reading a thread, so it is random (50 bits).
package queryid

import (
	"crypto/rand"
	"strings"
)

// Crockford base32: no I, L, O or U, so IDs are easy to read out and retype.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const codeLen = 10

// New returns a fresh random ID in canonical form.
func New() string {
	var b [codeLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("queryid: no randomness: " + err.Error())
	}
	var sb strings.Builder
	sb.WriteString("Q-")
	for i, v := range b {
		if i == 5 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(v)&31])
	}
	return sb.String()
}

// Normalize turns user input (any case, spaces, missing dashes, O/I/L typos)
// into the canonical form. ok is false if it cannot be a valid ID.
func Normalize(in string) (string, bool) {
	in = strings.ToUpper(strings.TrimSpace(in))
	var code []byte
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch c {
		case '-', ' ', '_':
			continue
		case 'O':
			c = '0'
		case 'I', 'L':
			c = '1'
		}
		if !strings.ContainsRune(alphabet, rune(c)) {
			return "", false
		}
		code = append(code, c)
	}
	if len(code) == codeLen+1 && code[0] == 'Q' { // the "Q-" prefix
		code = code[1:]
	}
	if len(code) != codeLen {
		return "", false
	}
	return "Q-" + string(code[:5]) + "-" + string(code[5:]), true
}
