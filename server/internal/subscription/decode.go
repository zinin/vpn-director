package subscription

import "strings"

// Decode reads a subscription body (spec 6.1): Xray JSON when it starts with
// [ or {, a plain link list when it holds "://", and otherwise base64 of a link
// list. Its error is one of ErrEmpty, ErrInvalidJSON and ErrUnrecognized, for
// a body it cannot read at all; a readable body whose entries were all skipped
// is a Result without servers.
func Decode(body string) (Result, error) {
	// bash drops NUL bytes from what it reads; so does this side.
	body = strings.ReplaceAll(body, "\x00", "")
	body = strings.TrimPrefix(body, "\ufeff")
	body = strings.Trim(body, asciiSpace)
	switch {
	case body == "":
		return Result{}, ErrEmpty
	case body[0] == '[' || body[0] == '{':
		return decodeXrayJSON(body)
	case strings.Contains(body, "://"):
		return decodeLinks(body)
	}
	text, ok := decodeBase64(body)
	if !ok {
		return Result{}, ErrUnrecognized
	}
	return decodeLinks(text)
}
