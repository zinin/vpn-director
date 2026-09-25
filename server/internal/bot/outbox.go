package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/telegram"
)

// The subscription watch's notifications wait in the outbox until a path to
// Telegram carries them. Where the WAN cannot reach Telegram, Xray's own SOCKS
// port is often the only way there, so the message that the outbound died is
// sent exactly when nothing can carry it; sent once, it was lost, and so was
// the one about the server the walk picked seconds later.
const (
	// outboxMaxPerChat is how many messages a chat keeps waiting; a long
	// outage drops the oldest.
	outboxMaxPerChat = 20
	// outboxMaxAge is how long a message stays worth delivering.
	outboxMaxAge = 12 * time.Hour
	// outboxLateAfter is how late a message may arrive before it says when it
	// happened.
	outboxLateAfter = time.Minute
	// outboxRetryEvery is how often the bot tries the waiting messages again.
	// A path comes back on the path manager's own schedule (every 30 s, or at
	// the next failure), so this bounds the delay after it.
	outboxRetryEvery = 10 * time.Second
)

type outboxNote struct {
	seq  uint64
	text string
	at   time.Time
}

// outbox holds each chat's waiting messages in the order they happened. The
// zero value is ready to use.
type outbox struct {
	now func() time.Time // nil => time.Now

	mu     sync.Mutex
	queues map[int64][]outboxNote
	seq    uint64

	// sending serializes flushes: two at once would send a chat's first
	// message twice.
	sending sync.Mutex
}

func (o *outbox) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// add queues text for every chat, stamped with the time it happened.
func (o *outbox) add(chats []int64, text string) {
	at := o.clock()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.queues == nil {
		o.queues = make(map[int64][]outboxNote)
	}
	for _, id := range chats {
		o.seq++
		q := append(o.queues[id], outboxNote{seq: o.seq, text: text, at: at})
		if over := len(q) - outboxMaxPerChat; over > 0 {
			slog.Warn("Telegram notifications dropped: too many waiting", "chat_id", id, "dropped", over)
			q = q[over:]
		}
		o.queues[id] = q
	}
}

// pending reports whether any message waits.
func (o *outbox) pending() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.queues) > 0
}

// flush sends each chat's messages in order. A send that failed on the way to
// Telegram stops that chat until the next flush; a message Telegram refused
// (the user blocked the bot, a bad request) is dropped, since a retry gets
// the same answer and would hold the chat's queue forever.
func (o *outbox) flush(sender telegram.MessageSender) {
	o.sending.Lock()
	defer o.sending.Unlock()
	for _, id := range o.chats() {
		for {
			n, ok := o.head(id)
			if !ok {
				break
			}
			now := o.clock()
			if age := now.Sub(n.at); age > outboxMaxAge {
				slog.Warn("Telegram notification dropped: older than 12 hours", "chat_id", id, "age", age.Round(time.Minute))
				o.pop(id, n.seq)
				continue
			}
			text, late := delayedText(n, now)
			if err := sender.SendPlain(id, text); err != nil && retryable(err) {
				break
			} else if err == nil && late {
				slog.Info("Delayed notification delivered", "chat_id", id, "age", now.Sub(n.at).Round(time.Second))
			}
			o.pop(id, n.seq)
		}
	}
}

func (o *outbox) chats() []int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	ids := make([]int64, 0, len(o.queues))
	for id := range o.queues {
		ids = append(ids, id)
	}
	return ids
}

func (o *outbox) head(id int64) (outboxNote, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	q := o.queues[id]
	if len(q) == 0 {
		return outboxNote{}, false
	}
	return q[0], true
}

// pop removes the chat's first message if it is still seq: an add that ran
// while it was being sent may have dropped it as the oldest.
func (o *outbox) pop(id int64, seq uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	q := o.queues[id]
	if len(q) == 0 || q[0].seq != seq {
		return
	}
	if len(q) == 1 {
		delete(o.queues, id)
		return
	}
	o.queues[id] = q[1:]
}

// delayedText is the message as sent at now: a message a minute or more late
// says when it happened, with the day when that was another day.
func delayedText(n outboxNote, now time.Time) (string, bool) {
	if now.Sub(n.at) < outboxLateAfter {
		return n.text, false
	}
	layout := "15:04"
	if y, m, d := n.at.Date(); !sameDay(y, m, d, now) {
		layout = "Jan 2 15:04"
	}
	return fmt.Sprintf("(%s, delayed) %s", n.at.Format(layout), n.text), true
}

func sameDay(y int, m time.Month, d int, t time.Time) bool {
	ty, tm, td := t.Date()
	return y == ty && m == tm && d == td
}

// retryable reports whether a failed send may succeed later: anything that
// failed on the way (no path, a timeout, a reset) and Telegram's own 429 and
// 5xx. Any other answer from Telegram is final.
func retryable(err error) bool {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusTooManyRequests || apiErr.Code >= http.StatusInternalServerError
	}
	return true
}

// flushNotifications sends the waiting messages once Telegram is connected and
// the path manager has a path to it; with no path every send would fail at
// once and log an error.
func (b *Bot) flushNotifications() {
	b.mu.Lock()
	sender := b.sender
	b.mu.Unlock()
	if sender == nil || !b.outbox.pending() {
		return
	}
	if b.pathLive != nil && !b.pathLive() {
		return
	}
	b.outbox.flush(sender)
}

// retryNotifications tries the waiting messages again every interval: once an
// outage ends, nothing else would send them.
func (b *Bot) retryNotifications(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.flushNotifications()
		}
	}
}
