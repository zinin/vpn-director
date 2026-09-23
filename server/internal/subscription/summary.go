package subscription

import (
	"fmt"
	"strings"
)

// Counts renders the non-zero skip and DNS counts in a fixed order, e.g.
// "7 composite, 1 DNS error". Empty when nothing was lost.
func (imp Import) Counts() string {
	count := map[string]int{}
	for _, s := range imp.Skipped {
		count[s.Reason]++
	}
	var parts []string
	for _, reason := range []string{ReasonUnsupported, ReasonComposite, ReasonInvalid} {
		if n := count[reason]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, reason))
		}
	}
	if n := count[ReasonPlaceholder]; n > 0 {
		parts = append(parts, plural(n, "placeholder"))
	}
	if imp.ResolveErrors > 0 {
		parts = append(parts, plural(imp.ResolveErrors, "DNS error"))
	}
	return strings.Join(parts, ", ")
}

// SkippedByReason counts the skips of each reason, every reason present.
func (imp Import) SkippedByReason() map[string]int {
	count := map[string]int{ReasonUnsupported: 0, ReasonComposite: 0, ReasonInvalid: 0, ReasonPlaceholder: 0}
	for _, s := range imp.Skipped {
		count[s.Reason]++
	}
	return count
}

// Details lists up to n "name: detail" lines for the unsupported and invalid
// entries - the skips a user can act on.
func (imp Import) Details(n int) []string {
	var lines []string
	for _, s := range imp.Skipped {
		if len(lines) == n {
			break
		}
		if s.Reason == ReasonUnsupported || s.Reason == ReasonInvalid {
			lines = append(lines, printable(s.Name+": "+s.Detail))
		}
	}
	return lines
}

// printable drops control characters - C0, DEL and C1 - from a string, the
// set JQ_PRINTABLE in import_server_list.sh drops. A skip's detail embeds
// subscription text as written, and cleanName keeps the C1 controls with the
// rest of U+0080-U+07FF, so either would otherwise take an escape sequence or
// a line break of its own to Telegram and the Web UI.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) {
			return -1
		}
		return r
	}, s)
}

// Summary is the one sentence the Web UI shows after an import:
// "Imported 32 of 40 servers: 7 composite, 1 DNS error", or
// "Imported 40 servers" when nothing was lost.
func (imp Import) Summary() string {
	counts := imp.Counts()
	if counts == "" {
		return fmt.Sprintf("Imported %d servers", len(imp.Servers))
	}
	return fmt.Sprintf("Imported %d of %d servers: %s", len(imp.Servers), imp.Total, counts)
}

// NoServers explains an import that decoded no server: the counts and up to
// three details.
func (imp Import) NoServers() string {
	msg := "no supported servers in subscription"
	if counts := imp.Counts(); counts != "" {
		msg += ": " + counts
	}
	if details := imp.Details(3); len(details) > 0 {
		msg += "; " + strings.Join(details, "; ")
	}
	return msg
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
