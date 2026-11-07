package jsonpatch

import (
	"fmt"
	"strings"
)

// jsonPointer is a parsed RFC 6901 JSON Pointer: its decoded reference tokens.
// The empty pointer addresses the whole document (root).
type jsonPointer []string

// parsePointer parses raw following RFC 6901, including empty reference tokens
// (object members named "") and the "~0"/"~1" escapes. A pointer deeper than
// maxDepth tokens is rejected.
func parsePointer(raw string, maxDepth int) (jsonPointer, error) {
	if raw == "" {
		return jsonPointer{}, nil
	}
	if raw[0] != '/' {
		return nil, fmt.Errorf("JSON Pointer %q must be empty (root) or start with '/'", raw)
	}
	tokens := strings.Split(raw[1:], "/")
	if len(tokens) > maxDepth {
		return nil, fmt.Errorf("JSON Pointer %q resolves through %d tokens, which exceeds the maximum depth of %d",
			raw, len(tokens), maxDepth)
	}
	for i, token := range tokens {
		decoded, err := decodePointerToken(token)
		if err != nil {
			return nil, fmt.Errorf("JSON Pointer %q: %w", raw, err)
		}
		tokens[i] = decoded
	}
	return jsonPointer(tokens), nil
}

// decodePointerToken decodes one reference token: '~1' is '/' and '~0' is '~',
// in that order. Any other use of '~' is malformed.
func decodePointerToken(token string) (string, error) {
	if !strings.ContainsRune(token, '~') {
		return token, nil
	}
	var b strings.Builder
	b.Grow(len(token))
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c != '~' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(token) {
			return "", fmt.Errorf("invalid escape %q at token %q: '~' must be followed by '0' or '1'", token[i:], token)
		}
		switch token[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", fmt.Errorf("invalid escape %q in token %q: only '~0' and '~1' are allowed", token[i:i+2], token)
		}
		i++
	}
	return b.String(), nil
}

// String renders the pointer back in RFC 6901 form.
func (p jsonPointer) String() string {
	var b strings.Builder
	for _, token := range p {
		b.WriteByte('/')
		b.WriteString(encodePointerToken(token))
	}
	return b.String()
}

func encodePointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")
	return token
}

// isProperPrefix reports whether child addresses a value strictly below
// ancestor: it has more tokens and shares all of ancestor's tokens.
func isProperPrefix(ancestor, child jsonPointer) bool {
	if len(child) <= len(ancestor) {
		return false
	}
	for i := range ancestor {
		if ancestor[i] != child[i] {
			return false
		}
	}
	return true
}

// arrayTokenError is returned when a reference token is used against an
// array. Malformed tokens (non-digits or a leading zero) can never address an
// array element and violate RFC 6901; the other cases ("-" outside add, or an
// index past the array) are structurally valid tokens that simply resolve to
// no existing element.
type arrayTokenError struct {
	malformed bool
	reason    string
}

func (e *arrayTokenError) Error() string { return e.reason }

// parseArrayIndex parses a reference token that addresses an array element.
// RFC 6901 forbids leading zeroes ("01"). When append is true (add operation)
// the token "-" denotes the one-past-last position and an index up to the
// array length is allowed; otherwise an existing element must be addressed.
func parseArrayIndex(token string, length int, append bool) (int, error) {
	if token == "-" {
		if append {
			return length, nil
		}
		return -1, &arrayTokenError{reason: `"-" only addresses the member after the last array element, so it cannot address an existing element`}
	}
	if len(token) == 0 {
		return -1, &arrayTokenError{malformed: true, reason: "invalid array index: empty reference token"}
	}
	if len(token) > 1 && token[0] == '0' {
		return -1, &arrayTokenError{malformed: true, reason: fmt.Sprintf("array index %q has a leading zero", token)}
	}
	idx := 0
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c < '0' || c > '9' {
			return -1, &arrayTokenError{malformed: true, reason: fmt.Sprintf("invalid array index %q: reference token is not all digits", token)}
		}
		idx = idx*10 + int(c-'0')
		// Bound early so absurdly long digit strings cannot overflow.
		if idx > length {
			break
		}
	}
	if append {
		if idx > length {
			return -1, &arrayTokenError{reason: fmt.Sprintf("array index %d is out of bounds for insertion into an array of length %d", idx, length)}
		}
		return idx, nil
	}
	if idx >= length {
		return -1, &arrayTokenError{reason: fmt.Sprintf("array index %d is out of bounds for an array of length %d", idx, length)}
	}
	return idx, nil
}
