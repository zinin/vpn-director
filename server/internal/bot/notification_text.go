package bot

import (
	"fmt"
	"time"

	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func notificationText(n watchdapi.Notification, now time.Time) string {
	if now.Sub(n.At) < notificationLateAfter {
		return n.Text
	}
	layout := "15:04"
	if y, m, d := n.At.Date(); !sameDay(y, m, d, now) {
		layout = "Jan 2 15:04"
	}
	return fmt.Sprintf("(%s, delayed) %s", n.At.Format(layout), n.Text)
}

func sameDay(y int, m time.Month, d int, t time.Time) bool {
	ty, tm, td := t.Date()
	return y == ty && m == tm && d == td
}

// notificationBlock splits at rune boundaries without copying the entire text.
func notificationBlock(text string) (string, string) {
	end, count, newline, newlineRune := len(text), 0, 0, 0
	for i, r := range text {
		if count == telegram.MaxMessageLength {
			end = i
			break
		}
		if r == '\n' {
			newline, newlineRune = i+1, count
		}
		count++
	}
	if end < len(text) && newlineRune > telegram.MaxMessageLength/2 {
		end = newline
	}
	return text[:end], text[end:]
}
