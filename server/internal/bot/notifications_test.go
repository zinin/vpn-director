package bot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/chatstore"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

type receiverAck struct {
	chatID  int64
	eventID watchdapi.EventID
}

// receiverAPI replaces only the external watchd transport.
type receiverAPI struct {
	mu         sync.Mutex
	recipients [][]watchdapi.Recipient
	cursors    []string
	acks       []receiverAck
	set        func(context.Context, []watchdapi.Recipient) error
	pending    func(context.Context, string) (watchdapi.NotificationPage, error)
	ack        func(context.Context, int64, watchdapi.EventID) error
}

var _ watchdapi.NotificationAPI = (*receiverAPI)(nil)

func (a *receiverAPI) SetRecipients(ctx context.Context, recipients []watchdapi.Recipient) error {
	copyOf := append([]watchdapi.Recipient{}, recipients...)
	a.mu.Lock()
	a.recipients = append(a.recipients, copyOf)
	a.mu.Unlock()
	if a.set != nil {
		return a.set(ctx, copyOf)
	}
	return nil
}

func (a *receiverAPI) Pending(ctx context.Context, cursor string) (watchdapi.NotificationPage, error) {
	a.mu.Lock()
	a.cursors = append(a.cursors, cursor)
	a.mu.Unlock()
	if a.pending != nil {
		return a.pending(ctx, cursor)
	}
	return watchdapi.NotificationPage{Messages: []watchdapi.Notification{}}, nil
}

func (a *receiverAPI) Ack(ctx context.Context, chatID int64, eventID watchdapi.EventID) error {
	a.mu.Lock()
	a.acks = append(a.acks, receiverAck{chatID: chatID, eventID: eventID})
	a.mu.Unlock()
	if a.ack != nil {
		return a.ack(ctx, chatID, eventID)
	}
	return nil
}

func (a *receiverAPI) snapshot() ([][]watchdapi.Recipient, []string, []receiverAck) {
	a.mu.Lock()
	defer a.mu.Unlock()
	recipients := make([][]watchdapi.Recipient, len(a.recipients))
	for i, r := range a.recipients {
		recipients[i] = append([]watchdapi.Recipient{}, r...)
	}
	return recipients, append([]string{}, a.cursors...), append([]receiverAck{}, a.acks...)
}

// receiverTelegram leaves plain-text chunking in the real telegram.Sender.
type receiverTelegram struct {
	mu        sync.Mutex
	attempts  []tgbotapi.MessageConfig
	delivered []tgbotapi.MessageConfig
	send      func(tgbotapi.MessageConfig) error
}

func (s *receiverTelegram) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	message, ok := c.(tgbotapi.MessageConfig)
	if !ok {
		return tgbotapi.Message{}, errors.New("expected a plain notification message")
	}
	s.mu.Lock()
	s.attempts = append(s.attempts, message)
	s.mu.Unlock()
	if s.send != nil {
		if err := s.send(message); err != nil {
			return tgbotapi.Message{}, err
		}
	}
	s.mu.Lock()
	s.delivered = append(s.delivered, message)
	s.mu.Unlock()
	return tgbotapi.Message{MessageID: 1}, nil
}

func (s *receiverTelegram) Request(tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	return &tgbotapi.APIResponse{Ok: true}, nil
}

func (s *receiverTelegram) texts(chatID int64, attempted bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages := s.delivered
	if attempted {
		messages = s.attempts
	}
	texts := []string{}
	for _, message := range messages {
		if message.ChatID == chatID {
			texts = append(texts, message.Text)
		}
	}
	return texts
}

func (s *receiverTelegram) messages() []tgbotapi.MessageConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tgbotapi.MessageConfig{}, s.delivered...)
}

func receiverChatStore(t *testing.T, data string) *chatstore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chats.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return chatstore.New(path)
}

func receiverBot(t *testing.T, api watchdapi.NotificationAPI, sender telegram.MessageSender) *Bot {
	t.Helper()
	store := receiverChatStore(t, `{
		"alice":{"chat_id":100,"first_seen":"2026-01-02T15:04:00Z","last_seen":"2026-01-02T15:04:00Z","active":true,"notified_versions":[]},
		"alice_renamed":{"chat_id":100,"first_seen":"2026-01-03T15:04:00Z","last_seen":"2026-01-03T15:04:00Z","active":true,"notified_versions":[]},
		"bob":{"chat_id":200,"first_seen":"2026-01-02T15:04:00Z","last_seen":"2026-01-02T15:04:00Z","active":true,"notified_versions":[]}
	}`)
	b := &Bot{auth: NewAuth([]string{"alice", "alice_renamed", "bob"}), chatStore: store, sender: sender}
	withNotifications(api)(b)
	return b
}

func receiverNote(chatID int64, id watchdapi.EventID, text string) watchdapi.Notification {
	return watchdapi.Notification{ChatID: chatID, EventID: id, At: time.Now(), Text: text}
}

func receiverPoll(b *Bot) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return b.pollNotifications(ctx)
}

func receiverWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestRecipients_AuthorizedEarliestFirstSeen(t *testing.T) {
	store := receiverChatStore(t, `{
		"alice":{"chat_id":100,"first_seen":"2026-01-03T15:04:00Z","last_seen":"2026-01-03T15:04:00Z","active":true,"notified_versions":[]},
		"alice_renamed":{"chat_id":100,"first_seen":"2026-01-02T15:04:00Z","last_seen":"2026-01-02T15:04:00Z","active":true,"notified_versions":[]},
		"inactive_alias":{"chat_id":100,"first_seen":"2026-01-01T15:04:00Z","last_seen":"2026-01-01T15:04:00Z","active":false,"notified_versions":[]},
		"mallory":{"chat_id":100,"first_seen":"2025-12-31T15:04:00Z","last_seen":"2025-12-31T15:04:00Z","active":true,"notified_versions":[]},
		"bob":{"chat_id":200,"first_seen":"2026-01-04T15:04:00Z","last_seen":"2026-01-04T15:04:00Z","active":true,"notified_versions":[]},
		"outsider":{"chat_id":300,"first_seen":"2026-01-01T15:04:00Z","last_seen":"2026-01-01T15:04:00Z","active":true,"notified_versions":[]}
	}`)
	auth := NewAuth([]string{"ALICE", "alice_renamed", "inactive_alias", "bob"})
	got := activeRecipients(store, auth)
	sort.Slice(got, func(i, j int) bool { return got[i].ChatID < got[j].ChatID })
	want := []watchdapi.Recipient{
		{ChatID: 100, FirstSeen: time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC)},
		{ChatID: 200, FirstSeen: time.Date(2026, 1, 4, 15, 4, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recipients = %+v, want %+v", got, want)
	}
	if got := activeRecipients(nil, auth); len(got) != 0 {
		t.Fatalf("nil store produced recipients %+v", got)
	}
	if got := activeRecipients(store, nil); len(got) != 0 {
		t.Fatalf("nil authorization produced recipients %+v", got)
	}
}

func TestReceiver_AckRetryAndIndependentChats(t *testing.T) {
	t.Run("successful_send_failed_ack_then_empty_pending", func(t *testing.T) {
		note := receiverNote(100, "11111111111111111111111111111111:1", "Xray outbound is dead")
		var reads, ackAttempts atomic.Int32
		api := &receiverAPI{
			pending: func(context.Context, string) (watchdapi.NotificationPage, error) {
				if reads.Add(1) == 1 {
					return watchdapi.NotificationPage{Messages: []watchdapi.Notification{note}}, nil
				}
				return watchdapi.NotificationPage{Messages: []watchdapi.Notification{}}, nil
			},
			ack: func(context.Context, int64, watchdapi.EventID) error {
				if ackAttempts.Add(1) == 1 {
					return errors.New("ack storage unavailable")
				}
				return nil
			},
		}
		transport := &receiverTelegram{}
		b := receiverBot(t, api, telegram.NewSender(transport))
		_ = receiverPoll(b)
		if sendCount := len(transport.texts(100, true)); sendCount != 1 || ackAttempts.Load() != 1 {
			t.Fatalf("after first poll: sendCount=%d ackAttempts=%d, want 1/1", sendCount, ackAttempts.Load())
		}
		if err := receiverPoll(b); err != nil {
			t.Fatal(err)
		}
		if sendCount := len(transport.texts(100, true)); sendCount != 1 || ackAttempts.Load() != 2 {
			t.Fatalf("after empty pending: sendCount=%d ackAttempts=%d, want 1/2", sendCount, ackAttempts.Load())
		}
		_, _, acks := api.snapshot()
		want := []receiverAck{{100, note.EventID}, {100, note.EventID}}
		if !reflect.DeepEqual(acks, want) {
			t.Fatalf("acks = %+v, want %+v", acks, want)
		}
	})

	t.Run("slow_chat_does_not_hold_another_or_overlap_its_own_sends", func(t *testing.T) {
		firstStarted, secondAcked, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce, firstOnce, secondOnce sync.Once
		var inFirst, overlap atomic.Bool
		first := receiverNote(100, "11111111111111111111111111111111:1", "outage")
		otherChat := first
		otherChat.ChatID = 200
		notes := []watchdapi.Notification{first, receiverNote(100, "11111111111111111111111111111111:2", "recovered"), otherChat}
		api := &receiverAPI{
			pending: func(context.Context, string) (watchdapi.NotificationPage, error) {
				return watchdapi.NotificationPage{Messages: notes}, nil
			},
			ack: func(_ context.Context, chatID int64, _ watchdapi.EventID) error {
				if chatID == 200 {
					secondOnce.Do(func() { close(secondAcked) })
				}
				return nil
			},
		}
		transport := &receiverTelegram{send: func(message tgbotapi.MessageConfig) error {
			if message.ChatID == 100 && message.Text == "outage" {
				if inFirst.Swap(true) {
					overlap.Store(true)
				}
				firstOnce.Do(func() { close(firstStarted) })
				<-release
				inFirst.Store(false)
			} else if message.ChatID == 100 && inFirst.Load() {
				overlap.Store(true)
			}
			return nil
		}}
		b := receiverBot(t, api, telegram.NewSender(transport))
		done := make(chan error, 1)
		finished := make(chan struct{})
		go func() {
			done <- receiverPoll(b)
			close(finished)
		}()
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(release) })
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Error("poll did not finish after releasing the slow chat")
			}
		})
		receiverWait(t, firstStarted, "the slow chat's first send")
		receiverWait(t, secondAcked, "the independent chat's delivery and ack")
		if got := transport.texts(100, true); !reflect.DeepEqual(got, []string{"outage"}) {
			t.Fatalf("slow chat attempts before release = %q", got)
		}
		releaseOnce.Do(func() { close(release) })
		receiverWait(t, finished, "the completed poll")
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if overlap.Load() || !reflect.DeepEqual(transport.texts(100, false), []string{"outage", "recovered"}) {
			t.Fatalf("same-chat overlap=%v, delivered=%q", overlap.Load(), transport.texts(100, false))
		}
		if got := transport.texts(200, false); !reflect.DeepEqual(got, []string{"outage"}) {
			t.Fatalf("independent delivery = %q", got)
		}
	})

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport", errors.New("path unavailable")},
		{"429", &tgbotapi.Error{Code: 429, Message: "Too Many Requests"}},
		{"5xx", &tgbotapi.Error{Code: 502, Message: "Bad Gateway"}},
	} {
		t.Run(tc.name+"_preserves_order", func(t *testing.T) {
			notes := []watchdapi.Notification{
				receiverNote(100, "11111111111111111111111111111111:1", "Xray outbound is dead"),
				receiverNote(100, "11111111111111111111111111111111:2", "Xray moved to Beta / Germany-1"),
			}
			api := &receiverAPI{pending: func(context.Context, string) (watchdapi.NotificationPage, error) {
				return watchdapi.NotificationPage{Messages: notes}, nil
			}}
			var failed atomic.Bool
			transport := &receiverTelegram{send: func(message tgbotapi.MessageConfig) error {
				if message.Text == "Xray outbound is dead" && failed.CompareAndSwap(false, true) {
					return tc.err
				}
				return nil
			}}
			b := receiverBot(t, api, telegram.NewSender(transport))
			_ = receiverPoll(b)
			_, _, acks := api.snapshot()
			if got := transport.texts(100, true); !reflect.DeepEqual(got, []string{"Xray outbound is dead"}) || len(acks) != 0 {
				t.Fatalf("failed pass: attempts=%q acks=%+v", got, acks)
			}
			if err := receiverPoll(b); err != nil {
				t.Fatal(err)
			}
			want := []string{"Xray outbound is dead", "Xray moved to Beta / Germany-1"}
			if got := transport.texts(100, false); !reflect.DeepEqual(got, want) {
				t.Fatalf("delivered = %q, want %q", got, want)
			}
			_, _, acks = api.snapshot()
			if want := []receiverAck{{100, notes[0].EventID}, {100, notes[1].EventID}}; !reflect.DeepEqual(acks, want) {
				t.Fatalf("acks = %+v, want %+v", acks, want)
			}
		})
	}
}

func TestReceiver_RevocationAndPaging(t *testing.T) {
	for _, revoke := range []string{"authorization", "inactive"} {
		t.Run(revoke+"_changes_during_pending_read", func(t *testing.T) {
			transport := &receiverTelegram{}
			api := &receiverAPI{}
			b := receiverBot(t, api, telegram.NewSender(transport))
			api.pending = func(context.Context, string) (watchdapi.NotificationPage, error) {
				if revoke == "authorization" {
					b.mu.Lock()
					b.auth = NewAuth([]string{"bob"})
					b.mu.Unlock()
				} else if err := b.chatStore.SetInactiveChat(100); err != nil {
					return watchdapi.NotificationPage{}, err
				}
				return watchdapi.NotificationPage{Messages: []watchdapi.Notification{
					receiverNote(100, "11111111111111111111111111111111:1", "must not send"),
				}}, nil
			}
			_ = receiverPoll(b)
			if sendCount := len(transport.texts(100, true)); sendCount != 0 {
				t.Fatalf("revoked recipient sendCount = %d, want 0", sendCount)
			}
			if err := b.syncRecipients(context.Background()); err != nil {
				t.Fatal(err)
			}
			recipients, _, _ := api.snapshot()
			if len(recipients) == 0 {
				t.Fatal("no recipient replacement was sent")
			}
			last := recipients[len(recipients)-1]
			if len(last) != 1 || last[0].ChatID != 200 {
				t.Fatalf("recipient replacement after revoke = %+v, want only chat 200", last)
			}
		})
	}

	t.Run("authorization_is_rechecked_between_same_chat_messages", func(t *testing.T) {
		api := &receiverAPI{pending: func(context.Context, string) (watchdapi.NotificationPage, error) {
			return watchdapi.NotificationPage{Messages: []watchdapi.Notification{
				receiverNote(100, "11111111111111111111111111111111:1", "first"),
				receiverNote(100, "11111111111111111111111111111111:2", "revoked"),
			}}, nil
		}}
		transport := &receiverTelegram{}
		b := receiverBot(t, api, telegram.NewSender(transport))
		transport.send = func(tgbotapi.MessageConfig) error {
			b.mu.Lock()
			b.auth = NewAuth([]string{"bob"})
			b.mu.Unlock()
			return nil
		}
		_ = receiverPoll(b)
		if got := transport.texts(100, true); !reflect.DeepEqual(got, []string{"first"}) {
			t.Fatalf("attempts = %q, want only the pre-revocation send", got)
		}
	})

	t.Run("empty_replacement_revokes_every_recipient", func(t *testing.T) {
		api := &receiverAPI{}
		b := receiverBot(t, api, nil)
		if err := b.syncRecipients(context.Background()); err != nil {
			t.Fatal(err)
		}
		b.mu.Lock()
		b.auth = NewAuth(nil)
		b.mu.Unlock()
		if err := b.syncRecipients(context.Background()); err != nil {
			t.Fatal(err)
		}
		recipients, _, _ := api.snapshot()
		if len(recipients) != 2 || len(recipients[0]) != 2 || len(recipients[1]) != 0 {
			t.Fatalf("full replacements = %+v, want registered chats then an explicit empty replacement", recipients)
		}
	})

	for _, failPage := range []bool{false, true} {
		name := "completed_page"
		if failPage {
			name = "failed_page"
		}
		t.Run(name+"_next_cycle_starts_empty", func(t *testing.T) {
			const cursor = "11111111111111111111111111111111:100:1"
			first := receiverNote(100, "11111111111111111111111111111111:1", "first page")
			second := receiverNote(100, "11111111111111111111111111111111:2", "second page")
			next := receiverNote(100, "11111111111111111111111111111111:3", "new cycle")
			var cycles atomic.Int32
			var firstAcked atomic.Bool
			api := &receiverAPI{
				pending: func(_ context.Context, got string) (watchdapi.NotificationPage, error) {
					if got == "" {
						if cycles.Add(1) == 1 {
							return watchdapi.NotificationPage{Messages: []watchdapi.Notification{first}, NextCursor: cursor}, nil
						}
						messages := []watchdapi.Notification{next}
						if !firstAcked.Load() {
							messages = []watchdapi.Notification{first, next}
						}
						return watchdapi.NotificationPage{Messages: messages}, nil
					}
					if failPage {
						return watchdapi.NotificationPage{}, errors.New("page temporarily unavailable")
					}
					return watchdapi.NotificationPage{Messages: []watchdapi.Notification{second}}, nil
				},
				ack: func(_ context.Context, _ int64, id watchdapi.EventID) error {
					if id == first.EventID {
						firstAcked.Store(true)
					}
					return nil
				},
			}
			transport := &receiverTelegram{}
			b := receiverBot(t, api, telegram.NewSender(transport))
			_ = receiverPoll(b)
			if err := receiverPoll(b); err != nil {
				t.Fatal(err)
			}
			recipients, cursors, _ := api.snapshot()
			if want := []string{"", cursor, ""}; !reflect.DeepEqual(cursors, want) {
				t.Fatalf("cursors = %q, want %q", cursors, want)
			}
			if len(recipients) < 2 {
				t.Fatalf("recipient sync calls = %d, want a replacement every cycle", len(recipients))
			}
			want := []string{"first page", "second page", "new cycle"}
			if failPage {
				want = []string{"first page", "new cycle"}
			}
			if got := transport.texts(100, false); !reflect.DeepEqual(got, want) {
				t.Fatalf("delivery = %q, want %q", got, want)
			}
		})
	}
}

func TestReceiver_PermanentErrorsDeactivateBlockedAliasesOnly(t *testing.T) {
	for _, code := range []int{403, 400} {
		name := "blocked"
		if code == 400 {
			name = "bad_request"
		}
		t.Run(name, func(t *testing.T) {
			api := &receiverAPI{pending: func(context.Context, string) (watchdapi.NotificationPage, error) {
				return watchdapi.NotificationPage{Messages: []watchdapi.Notification{
					receiverNote(100, "11111111111111111111111111111111:1", "refused"),
					receiverNote(100, "11111111111111111111111111111111:2", "newer"),
					receiverNote(200, "11111111111111111111111111111111:3", "independent"),
				}}, nil
			}}
			transport := &receiverTelegram{send: func(message tgbotapi.MessageConfig) error {
				if message.ChatID == 100 && message.Text == "refused" {
					return &tgbotapi.Error{Code: code, Message: "synthetic permanent refusal"}
				}
				return nil
			}}
			b := receiverBot(t, api, telegram.NewSender(transport))
			_ = receiverPoll(b)
			if got := transport.texts(200, false); !reflect.DeepEqual(got, []string{"independent"}) {
				t.Fatalf("unrelated chat delivery = %q", got)
			}
			wantAttempts := []string{"refused", "newer"}
			wantUsers := 3
			if code == 403 {
				wantAttempts = []string{"refused"}
				wantUsers = 1
			}
			if got := transport.texts(100, true); !reflect.DeepEqual(got, wantAttempts) {
				t.Fatalf("permanently refused chat attempts = %q, want %q", got, wantAttempts)
			}
			users, err := b.chatStore.GetActiveUsers()
			if err != nil || len(users) != wantUsers {
				t.Fatalf("active users = %+v, %v; want %d", users, err, wantUsers)
			}
			if code == 403 {
				recipients, _, _ := api.snapshot()
				if len(recipients) == 0 {
					t.Fatal("no recipient replacement was sent")
				}
				last := recipients[len(recipients)-1]
				if len(last) != 1 || last[0].ChatID != 200 {
					t.Fatalf("blocked chat was not immediately revoked: %+v", last)
				}
			}
		})
	}
}

func TestReceiver_PathAndExpiryPreserveLegacyDelivery(t *testing.T) {
	transport := &receiverTelegram{}
	var live atomic.Bool
	expired := receiverNote(100, "11111111111111111111111111111111:1", "expired")
	expired.At = time.Now().Add(-12*time.Hour - time.Minute)
	notes := []watchdapi.Notification{
		expired, receiverNote(100, "11111111111111111111111111111111:2", "LAN clients back on Xray"),
	}
	api := &receiverAPI{pending: func(context.Context, string) (watchdapi.NotificationPage, error) {
		return watchdapi.NotificationPage{Messages: notes}, nil
	}}
	b := receiverBot(t, api, nil)
	b.pathLive = live.Load
	_ = receiverPoll(b)
	b.setSender(telegram.NewSender(transport))
	_ = receiverPoll(b)
	if got := transport.texts(100, true); len(got) != 0 {
		t.Fatalf("delivery attempted without a live path: %q", got)
	}
	live.Store(true)
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	if got := transport.texts(100, false); !reflect.DeepEqual(got, []string{"LAN clients back on Xray"}) {
		t.Fatalf("delivery after connection/path recovery = %q", got)
	}
}

func TestReceiver_RevocationForgetsSentAckRetries(t *testing.T) {
	var reads atomic.Int32
	api := &receiverAPI{
		pending: func(context.Context, string) (watchdapi.NotificationPage, error) {
			if reads.Add(1) == 1 {
				return watchdapi.NotificationPage{Messages: []watchdapi.Notification{
					receiverNote(100, "11111111111111111111111111111111:1", "delivered before revoke"),
				}}, nil
			}
			return watchdapi.NotificationPage{Messages: []watchdapi.Notification{}}, nil
		},
		ack: func(context.Context, int64, watchdapi.EventID) error { return errors.New("ack storage unavailable") },
	}
	transport := &receiverTelegram{}
	b := receiverBot(t, api, telegram.NewSender(transport))
	_ = receiverPoll(b)
	b.mu.Lock()
	b.auth = NewAuth([]string{"bob"})
	b.mu.Unlock()
	_ = receiverPoll(b)
	_ = receiverPoll(b)
	_, _, acks := api.snapshot()
	if len(transport.texts(100, true)) != 1 || len(acks) != 1 {
		t.Fatalf("revoked sent-event retention: sends=%q acks=%+v, want one send and no ack retry", transport.texts(100, true), acks)
	}
}

func TestReceiver_BlocksAckOnlyAfterAllSuccessful(t *testing.T) {
	note := receiverNote(100, "11111111111111111111111111111111:1", strings.Repeat("Я", telegram.MaxMessageLength)+" END")
	api := &receiverAPI{pending: func(context.Context, string) (watchdapi.NotificationPage, error) {
		return watchdapi.NotificationPage{Messages: []watchdapi.Notification{note}}, nil
	}}
	var attempts atomic.Int32
	transport := &receiverTelegram{send: func(tgbotapi.MessageConfig) error {
		if attempts.Add(1) == 2 {
			return &tgbotapi.Error{Code: 502, Message: "Bad Gateway"}
		}
		return nil
	}}
	b := receiverBot(t, api, telegram.NewSender(transport))
	_ = receiverPoll(b)
	_, _, acks := api.snapshot()
	if attempts.Load() != 2 || len(acks) != 0 {
		t.Fatalf("failed second block: attempts=%d acks=%+v; partial delivery must not ack", attempts.Load(), acks)
	}
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	_, _, acks = api.snapshot()
	if want := []receiverAck{{100, note.EventID}}; !reflect.DeepEqual(acks, want) {
		t.Fatalf("acks after complete delivery = %+v, want %+v", acks, want)
	}
	if got := strings.Join(transport.texts(100, false), ""); !strings.HasSuffix(got, note.Text) {
		t.Fatal("notification was acknowledged without delivering all its text")
	}
}

func TestNotificationText_MinuteBoundary(t *testing.T) {
	at := time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		now  time.Time
		want string
	}{
		{"59s", at.Add(59 * time.Second), "Xray outbound is dead"},
		{"60s", at.Add(60 * time.Second), "(15:04, delayed) Xray outbound is dead"},
		{"another_day", time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), "(Jan 2 15:04, delayed) Xray outbound is dead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := watchdapi.Notification{ChatID: 100, EventID: "11111111111111111111111111111111:1", At: at, Text: "Xray outbound is dead"}
			if got := notificationText(note, tc.now); got != tc.want {
				t.Fatalf("text = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("every_telegram_block_is_bounded_plain_utf8", func(t *testing.T) {
		note := receiverNote(100, "11111111111111111111111111111111:1", strings.Repeat("Я", telegram.MaxMessageLength)+" END")
		note.At = note.At.Add(-time.Minute)
		transport := &receiverTelegram{}
		var ackBeforeAll atomic.Bool
		api := &receiverAPI{
			pending: func(context.Context, string) (watchdapi.NotificationPage, error) {
				return watchdapi.NotificationPage{Messages: []watchdapi.Notification{note}}, nil
			},
			ack: func(context.Context, int64, watchdapi.EventID) error {
				if got := strings.Join(transport.texts(100, false), ""); !strings.HasSuffix(got, note.Text) {
					ackBeforeAll.Store(true)
				}
				return nil
			},
		}
		b := receiverBot(t, api, telegram.NewSender(transport))
		if err := receiverPoll(b); err != nil {
			t.Fatal(err)
		}
		messages := transport.messages()
		if len(messages) < 2 || ackBeforeAll.Load() {
			t.Fatalf("delivered blocks=%d, premature ack=%v", len(messages), ackBeforeAll.Load())
		}
		for i, message := range messages {
			if message.ParseMode != "" || !utf8.ValidString(message.Text) || utf8.RuneCountInString(message.Text) > telegram.MaxMessageLength {
				t.Fatalf("block %d: mode=%q, valid UTF8=%v, runes=%d", i, message.ParseMode, utf8.ValidString(message.Text), utf8.RuneCountInString(message.Text))
			}
		}
		joined := strings.Join(transport.texts(100, false), "")
		prefix := strings.TrimSuffix(joined, note.Text)
		if !strings.HasPrefix(prefix, "(") || !strings.HasSuffix(prefix, ", delayed) ") {
			t.Fatalf("delayed prefix lost or body changed: %q", prefix)
		}
		_, _, acks := api.snapshot()
		if want := []receiverAck{{100, note.EventID}}; !reflect.DeepEqual(acks, want) {
			t.Fatalf("bounded delivery acks = %+v, want %+v", acks, want)
		}
	})
}

func TestReceiver_PollsAndSyncsActivityEveryTenSeconds(t *testing.T) {
	reads := make(chan time.Time, 4)
	api := &receiverAPI{pending: func(ctx context.Context, _ string) (watchdapi.NotificationPage, error) {
		select {
		case reads <- time.Now():
		case <-ctx.Done():
			return watchdapi.NotificationPage{}, ctx.Err()
		}
		return watchdapi.NotificationPage{Messages: []watchdapi.Notification{}}, nil
	}}
	b := receiverBot(t, api, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.receiveNotifications(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("receiver did not stop with its context")
		}
	})
	var first time.Time
	select {
	case first = <-reads:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start its first poll")
	}
	if err := b.chatStore.RecordInteraction("new_user", 300); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.auth = NewAuth([]string{"alice", "alice_renamed", "bob", "new_user"})
	b.mu.Unlock()
	select {
	case second := <-reads:
		if elapsed := second.Sub(first); elapsed < 9*time.Second || elapsed > 12*time.Second {
			t.Fatalf("poll spacing = %v, want the 10-second schedule", elapsed)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("receiver did not perform the scheduled poll")
	}
	recipients, cursors, _ := api.snapshot()
	if len(recipients) < 2 || !reflect.DeepEqual(cursors, []string{"", ""}) {
		t.Fatalf("periodic synchronization: recipients=%+v cursors=%q", recipients, cursors)
	}
	last := recipients[len(recipients)-1]
	sort.Slice(last, func(i, j int) bool { return last[i].ChatID < last[j].ChatID })
	if len(last) != 3 || last[0].ChatID != 100 || last[1].ChatID != 200 || last[2].ChatID != 300 {
		t.Fatalf("new activity missing from scheduled recipient sync: %+v", last)
	}
}

func TestReceiver_SlowChatKeepsPeriodicPollAndSync(t *testing.T) {
	first := receiverNote(100, "11111111111111111111111111111111:1", "outage")
	follow := receiverNote(100, "11111111111111111111111111111111:2", "recovered")
	var newNote watchdapi.Notification
	var published, firstAcked, followAcked, newAcked atomic.Bool
	reads := make(chan time.Time, 4)
	firstStarted, release := make(chan struct{}), make(chan struct{})
	newDelivered, slowDelivered := make(chan struct{}), make(chan struct{})
	var firstOnce, releaseOnce, newOnce, slowOnce sync.Once
	var inFirst, overlap atomic.Bool
	api := &receiverAPI{
		pending: func(ctx context.Context, _ string) (watchdapi.NotificationPage, error) {
			messages := []watchdapi.Notification{}
			if !firstAcked.Load() {
				messages = append(messages, first)
			}
			if !followAcked.Load() {
				messages = append(messages, follow)
			}
			if published.Load() && !newAcked.Load() {
				messages = append(messages, newNote)
			}
			select {
			case reads <- time.Now():
			case <-ctx.Done():
				return watchdapi.NotificationPage{}, ctx.Err()
			}
			return watchdapi.NotificationPage{Messages: messages}, nil
		},
		ack: func(_ context.Context, chatID int64, eventID watchdapi.EventID) error {
			switch {
			case chatID == 100 && eventID == first.EventID:
				firstAcked.Store(true)
			case chatID == 100 && eventID == follow.EventID:
				followAcked.Store(true)
				slowOnce.Do(func() { close(slowDelivered) })
			case chatID == 300 && eventID == "11111111111111111111111111111111:3":
				newAcked.Store(true)
				newOnce.Do(func() { close(newDelivered) })
			}
			return nil
		},
	}
	transport := &receiverTelegram{send: func(message tgbotapi.MessageConfig) error {
		if message.ChatID == 100 && message.Text == first.Text {
			if inFirst.Swap(true) {
				overlap.Store(true)
			}
			firstOnce.Do(func() { close(firstStarted) })
			<-release
			inFirst.Store(false)
		} else if message.ChatID == 100 && inFirst.Load() {
			overlap.Store(true)
		}
		return nil
	}}
	b := receiverBot(t, api, telegram.NewSender(transport))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.receiveNotifications(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("receiver did not stop after releasing the slow send")
		}
	})
	var initialPoll time.Time
	select {
	case initialPoll = <-reads:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start its first poll")
	}
	receiverWait(t, firstStarted, "the blocked first chat send")
	if err := b.chatStore.RecordInteraction("new_user", 300); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.auth = NewAuth([]string{"alice", "alice_renamed", "bob", "new_user"})
	b.mu.Unlock()
	newNote = receiverNote(300, "11111111111111111111111111111111:3", "new independent event")
	published.Store(true)
	select {
	case nextPoll := <-reads:
		if elapsed := nextPoll.Sub(initialPoll); elapsed < 9*time.Second || elapsed > 12*time.Second {
			t.Fatalf("poll spacing during slow send = %v, want the 10-second schedule", elapsed)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("slow send prevented the next scheduled Pending and recipient sync")
	}
	recipients, cursors, _ := api.snapshot()
	if len(recipients) < 2 || !reflect.DeepEqual(cursors, []string{"", ""}) {
		t.Fatalf("polls while send is blocked: recipients=%+v cursors=%q", recipients, cursors)
	}
	last := recipients[len(recipients)-1]
	if len(last) != 3 || last[0].ChatID != 100 || last[1].ChatID != 200 || last[2].ChatID != 300 {
		t.Fatalf("recipient change was not synchronized during slow send: %+v", last)
	}
	receiverWait(t, newDelivered, "the new chat's delivery and ack before releasing the slow chat")
	if got := transport.texts(300, false); !reflect.DeepEqual(got, []string{"new independent event"}) {
		t.Fatalf("new chat delivery while slow send is blocked = %q", got)
	}
	if got := transport.texts(100, true); overlap.Load() || !reflect.DeepEqual(got, []string{"outage"}) {
		t.Fatalf("same-chat jobs overlapped across polls: overlap=%v attempts=%q", overlap.Load(), got)
	}
	_, _, acks := api.snapshot()
	if want := []receiverAck{{300, newNote.EventID}}; !reflect.DeepEqual(acks, want) {
		t.Fatalf("acks before slow send release = %+v, want %+v", acks, want)
	}
	releaseOnce.Do(func() { close(release) })
	receiverWait(t, slowDelivered, "the slow chat's FIFO delivery after release")
	cancel()
	receiverWait(t, done, "receiver cancellation")
	if got := transport.texts(100, false); overlap.Load() || !reflect.DeepEqual(got, []string{"outage", "recovered"}) {
		t.Fatalf("same-chat delivery after release: overlap=%v delivered=%q", overlap.Load(), got)
	}
	_, _, acks = api.snapshot()
	if want := []receiverAck{{300, newNote.EventID}, {100, first.EventID}, {100, follow.EventID}}; !reflect.DeepEqual(acks, want) {
		t.Fatalf("cross-poll delivery acks = %+v, want %+v", acks, want)
	}
}
