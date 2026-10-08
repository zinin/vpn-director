package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/chatstore"
	"github.com/zinin/vpn-director/server/internal/config"
	"github.com/zinin/vpn-director/server/internal/devmode"
	"github.com/zinin/vpn-director/server/internal/netpath"
	"github.com/zinin/vpn-director/server/internal/notifications"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/shell"
	"github.com/zinin/vpn-director/server/internal/subwatch"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// newAPIServer answers getMe the way Telegram does and everything else with an
// empty ok. One server stands for both the probe target and the API, as one
// host does in production.
func newAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test","username":"testbot"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	dir := t.TempDir()
	return paths.Paths{
		ScriptsDir:     dir,
		BotConfigPath:  dir + "/telegram-bot.json",
		DefaultDataDir: dir + "/data",
		XrayTemplate:   dir + "/xray.template.json",
		XrayConfig:     dir + "/xray.json",
		BotLogPath:     dir + "/bot.log",
		VPNLogPath:     dir + "/vpn.log",
		WebUILogPath:   dir + "/webui.log",
		XrayLogPath:    dir + "/xray-error.log",
	}
}

func testConfig() *config.Config {
	return &config.Config{BotToken: "123:TESTTOKEN", AllowedUsers: []string{"tester"}}
}

func TestNew_DevModeUsesPlainClientAndNoPathManager(t *testing.T) {
	srv := newAPIServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b, err := New(ctx, testConfig(), testPaths(t), "v0.0.0", "v0.0.0-test", "deadbee", "2026-01-01",
		WithDevMode(devmode.NewExecutor()), withAPIBase(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if b.pathManager != nil {
		t.Fatal("dev mode must not build a PathManager")
	}
	c, ok := b.api.Client.(*http.Client)
	if !ok {
		t.Fatalf("client is %T", b.api.Client)
	}
	// A nil Transport is http.DefaultTransport, which is what dev mode wants.
	if _, isPath := c.Transport.(*pathTransport); isPath {
		t.Fatal("dev mode must not dial through a pathTransport")
	}
}

type botOwnershipExecutor struct {
	botWatchStarts atomic.Int64
	started        chan struct{}
}

func (e *botOwnershipExecutor) Exec(ctx context.Context, _ string, args ...string) (*shell.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reflect.DeepEqual(args, []string{"platform"}) {
		return &shell.Result{Output: `{"platform":"merlin","tunnels":[]}`}, nil
	}
	e.botWatchStarts.Add(1)
	select {
	case e.started <- struct{}{}:
	default:
	}
	return &shell.Result{}, nil
}

func botRecoveryPaths(t *testing.T) (paths.Paths, *botOwnershipExecutor, []byte) {
	t.Helper()
	p := testPaths(t)
	raw := []byte(`{"xray":{"clients":["192.168.50.8"],"active_server":{"name":"Oslo","address":"oslo.example","port":443,"seq":7},"pending_restore":{"snapshot":{"tunnel":"wgc1","clients":["192.168.50.8"],"added":["192.168.50.8"],"committed":true},"restored":["192.168.50.8"],"active":{"name":"Oslo","address":"oslo.example","port":443,"seq":7}}}}`)
	if err := os.WriteFile(filepath.Join(p.ScriptsDir, "vpn-director.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return p, &botOwnershipExecutor{started: make(chan struct{}, 1)}, raw
}

func assertNoBotWatch(t *testing.T, p paths.Paths, executor *botOwnershipExecutor, before []byte) {
	t.Helper()
	select {
	case <-executor.started:
		t.Fatal("bot started routing recovery that belongs exclusively to watchd")
	case <-time.After(100 * time.Millisecond):
	}
	if botWatchStarts := executor.botWatchStarts.Load(); botWatchStarts != 0 {
		t.Fatalf("botWatchStarts=%d, want 0", botWatchStarts)
	}
	if after, err := os.ReadFile(filepath.Join(p.ScriptsDir, "vpn-director.json")); err != nil || string(after) != string(before) {
		t.Fatal("bot startup mutated persisted routing recovery")
	}
}

func TestNew_DevModeDoesNotStartWatch(t *testing.T) {
	srv := newAPIServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, executor, before := botRecoveryPaths(t)
	if _, err := New(ctx, testConfig(), p, "v0.0.0", "v0.0.0-test", "deadbee", "2026-01-01",
		WithDevMode(executor), withNotifications(&receiverAPI{}), withAPIBase(srv.URL)); err != nil {
		t.Fatal(err)
	}
	assertNoBotWatch(t, p, executor, before)
}

func TestNew_NoBotWatchWhenGetMeFailsAndReceiverLives(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":500,"description":"down"}`)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, executor, before := botRecoveryPaths(t)
	pendingStarted, pendingCanceled := make(chan struct{}), make(chan struct{})
	var first sync.Once
	api := &receiverAPI{pending: func(ctx context.Context, _ string) (watchdapi.NotificationPage, error) {
		first.Do(func() { close(pendingStarted) })
		<-ctx.Done()
		select {
		case <-pendingCanceled:
		default:
			close(pendingCanceled)
		}
		return watchdapi.NotificationPage{}, ctx.Err()
	}}
	b, err := New(ctx, testConfig(), p, "v0.0.0", "v0.0.0-test", "deadbee", "2026-01-01",
		func(b *Bot) { b.executor = executor }, withNotifications(api), withAPIBase(srv.URL))
	if err == nil || b == nil {
		t.Fatal("getMe must fail without discarding the receiver-owning bot")
	}
	receiverWait(t, pendingStarted, "live receiver despite failed getMe")
	if err := b.Connect(testConfig()); err == nil {
		t.Fatal("Connect retry must still report the Telegram failure")
	}
	assertNoBotWatch(t, p, executor, before)
	_, cursors, _ := api.snapshot()
	if !reflect.DeepEqual(cursors, []string{""}) {
		t.Fatalf("Connect retry restarted or killed the receiver: cursors=%q", cursors)
	}
	cancel()
	receiverWait(t, pendingCanceled, "receiver root cancellation")
}

func TestNew_BotReceiverCancellationKeepsWatchTicks(t *testing.T) {
	p := testPaths(t)
	cfg := service.NewConfigService(p.ScriptsDir, p.DefaultDataDir)
	before := []byte(`{"data_dir":"data","xray":{"clients":["192.168.50.8"]}}`)
	if err := os.WriteFile(cfg.ConfigPath(), before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveSubscription(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "static", Servers: []vpnconfig.Server{}}); err != nil {
		t.Fatal(err)
	}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	ticks := make(chan struct{}, 4)
	var watchTicks atomic.Int64
	watch := &subwatch.Watch{
		LoadVPN: cfg.LoadVPNConfig, UpdateVPN: cfg.UpdateVPNConfig,
		LoadSubscriptions: cfg.LoadSubscriptions,
		Probe: func(ctx context.Context, _ int) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			watchTicks.Add(1)
			select {
			case ticks <- struct{}{}:
			default:
			}
			return nil
		},
	}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		watch.Start(watchCtx)
	}()
	t.Cleanup(func() {
		stopWatch()
		receiverWait(t, watchDone, "independently owned watch shutdown")
	})
	receiverWait(t, ticks, "initial independent watch tick")

	botCtx, stopBot := context.WithCancel(context.Background())
	defer stopBot()
	pendingStarted := make(chan struct{})
	pendingEnded := make(chan error, 1)
	var first sync.Once
	api := &receiverAPI{pending: func(ctx context.Context, _ string) (watchdapi.NotificationPage, error) {
		first.Do(func() { close(pendingStarted) })
		<-ctx.Done()
		select {
		case pendingEnded <- ctx.Err():
		default:
		}
		return watchdapi.NotificationPage{}, ctx.Err()
	}}
	srv := newAPIServer(t)
	b, err := New(botCtx, testConfig(), p, "v0.0.0", "v0.0.0-test", "deadbee", "2026-01-01",
		WithDevMode(devmode.NewExecutor()), withNotifications(api), withAPIBase(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	receiverWait(t, pendingStarted, "constructor-owned bot receiver")
	receiverDone := make(chan struct{})
	go func() {
		// Once waits for the constructor's receiver and all delivery workers to drain.
		b.receiveNotifications(botCtx)
		close(receiverDone)
	}()
	t.Cleanup(func() {
		stopBot()
		receiverWait(t, receiverDone, "bot receiver cleanup")
	})
	stopBot()
	select {
	case err := <-pendingEnded:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("receiver ended through its request deadline instead of bot cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bot cancellation did not cancel the running receiver's IPC request")
	}
	receiverWait(t, receiverDone, "real bot receiver shutdown")
	ticksAtShutdown := watchTicks.Load()
	deadline := time.After(32 * time.Second)
	for watchTicks.Load() <= ticksAtShutdown {
		select {
		case <-ticks:
		case <-deadline:
			t.Fatal("watch did not run its next 30-second tick after real bot receiver shutdown")
		}
	}
	if watchTicks.Load() < 2 || watchCtx.Err() != nil {
		t.Fatalf("bot receiver cancellation stopped independent watch: ticks=%d context=%v", watchTicks.Load(), watchCtx.Err())
	}
	select {
	case <-watchDone:
		t.Fatal("bot receiver shutdown ended the watch lifetime")
	default:
	}
	if after, err := os.ReadFile(cfg.ConfigPath()); err != nil || string(after) != string(before) {
		t.Fatal("bot receiver shutdown changed current routing configuration")
	}
}

func TestNew_ProductionUsesPathClientAndManager(t *testing.T) {
	srv := newAPIServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, executor, before := botRecoveryPaths(t)

	b, err := New(ctx, testConfig(), p, "v0.0.0", "v0.0.0-test", "deadbee", "2026-01-01",
		func(b *Bot) { b.executor = executor }, withNotifications(&receiverAPI{}), withAPIBase(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if b.pathManager == nil {
		t.Fatal("production must build a PathManager")
	}
	var current netpath.Path = b.pathManager.Current()
	if got := current.String(); got != "direct" {
		t.Fatalf("path %q; the local server answers the probe, so direct is live", got)
	}
	c, ok := b.api.Client.(*http.Client)
	if !ok {
		t.Fatalf("client is %T", b.api.Client)
	}
	if _, isPath := c.Transport.(*pathTransport); !isPath {
		t.Fatalf("transport is %T; production must dial through a pathTransport", c.Transport)
	}
	running := func() bool {
		b.pathManager.mu.Lock()
		defer b.pathManager.mu.Unlock()
		return b.pathManager.running
	}
	deadline := time.Now().Add(time.Second)
	for !running() {
		if time.Now().After(deadline) {
			t.Fatal("path monitor must start before getMe so a blackhole can fail over")
		}
		time.Sleep(time.Millisecond)
	}
	assertNoBotWatch(t, p, executor, before)
}

func TestReceiver_QueuesUntilSender(t *testing.T) {
	transport := &receiverTelegram{}
	b, q, _ := legacyReceiver(t, nil, false, nil)
	legacyPublish(t, q, "Xray outbound is down")
	_ = receiverPoll(b)
	if got := transport.texts(100, true); len(got) != 0 {
		t.Fatalf("sent %q before Telegram connected", got)
	}
	b.setSender(telegram.NewSender(transport))
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	if got := transport.texts(100, false); !reflect.DeepEqual(got, []string{"Xray outbound is down"}) {
		t.Fatalf("delivered %q after connecting the sender", got)
	}
}

func TestReceiver_NoRaceWithSetSender(t *testing.T) {
	transport := &receiverTelegram{}
	b, q, _ := legacyReceiver(t, nil, false, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if _, err := q.Publish("Xray outbound is down"); err != nil {
				t.Error(err)
				return
			}
			_ = receiverPoll(b)
		}
	}()
	b.setSender(telegram.NewSender(transport))
	<-done
	legacyPublish(t, q, "LAN clients back on Xray")
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	if got := transport.texts(100, false); len(got) == 0 || got[len(got)-1] != "LAN clients back on Xray" {
		t.Fatal("expected the post-connect notification under concurrent sender publication")
	}
}

func TestReceiver_AuthorizedOncePerChat(t *testing.T) {
	store := chatstore.New(filepath.Join(t.TempDir(), "chats.json"))
	for _, rec := range []struct {
		username string
		chatID   int64
	}{
		{"mallory", 200},
		{"alice", 100},
		{"alice_renamed", 100},
	} {
		if err := store.RecordInteraction(rec.username, rec.chatID); err != nil {
			t.Fatal(err)
		}
	}
	q, err := notifications.NewStore(filepath.Join(t.TempDir(), "watchd-notifications.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &receiverTelegram{}
	b := &Bot{auth: NewAuth([]string{"alice", "alice_renamed"}), sender: telegram.NewSender(transport), chatStore: store, notificationAPI: queueNotificationAPI{q}}
	legacyPublish(t, q, "Xray outbound is down")
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	if got := transport.texts(100, false); !reflect.DeepEqual(got, []string{"Xray outbound is down"}) || len(transport.texts(200, true)) != 0 {
		t.Fatalf("authorized deduplicated delivery=%q, unauthorized=%q", got, transport.texts(200, true))
	}
}

func TestReceiver_NewChatOfAKnownUserStartsAtItsOwnAppearance(t *testing.T) {
	store := chatstore.New(filepath.Join(t.TempDir(), "chats.json"))
	q, err := notifications.NewStore(filepath.Join(t.TempDir(), "watchd-notifications.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{auth: NewAuth([]string{"alice"}), chatStore: store, notificationAPI: queueNotificationAPI{q}}
	if err := store.RecordInteraction("alice", 100); err != nil {
		t.Fatal(err)
	}
	if err := b.syncRecipients(context.Background()); err != nil {
		t.Fatal(err)
	}
	earlier := legacyPublish(t, q, "Xray outbound is down")
	page, err := q.Pending("")
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ChatID != 100 || page.Messages[0].EventID != earlier {
		t.Fatalf("private chat queue = %+v, %v; want the event", page, err)
	}

	before := time.Now()
	if err := store.RecordInteraction("alice", -500); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	recipients := b.notificationRecipients()
	if len(recipients) != 1 || recipients[0].ChatID != -500 || recipients[0].FirstSeen.Before(before) || recipients[0].FirstSeen.After(after) {
		t.Fatalf("recipients %+v, want chat -500 first seen at its interaction, between %v and %v", recipients, before, after)
	}
	if err := b.syncRecipients(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err = q.Pending("")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range page.Messages {
		if n.ChatID == -500 {
			t.Fatalf("the new chat was queued %q, published before it appeared", n.Text)
		}
	}
}

func TestReceiver_SyncBeforeGetMe(t *testing.T) {
	for _, failGetMe := range []bool{false, true} {
		name := "successful_getme"
		if failGetMe {
			name = "failed_getme_and_connect_retry"
		}
		t.Run(name, func(t *testing.T) {
			store := receiverChatStore(t, `{"tester":{"chat_id":100,"first_seen":"2026-01-02T15:04:00Z","last_seen":"2026-01-02T15:04:00Z","active":true,"notified_versions":[]}}`)
			want := []watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC)}}
			var synchronized, telegramBeforeSync atomic.Bool
			var telegramAttempts, pendingCalls atomic.Int32
			pendingStarted, release := make(chan struct{}), make(chan struct{})
			api := &receiverAPI{
				set: func(_ context.Context, recipients []watchdapi.Recipient) error {
					if reflect.DeepEqual(recipients, want) {
						synchronized.Store(true)
					}
					return nil
				},
				pending: func(ctx context.Context, _ string) (watchdapi.NotificationPage, error) {
					if pendingCalls.Add(1) == 1 {
						close(pendingStarted)
					}
					select {
					case <-ctx.Done():
						return watchdapi.NotificationPage{}, ctx.Err()
					case <-release:
						return watchdapi.NotificationPage{Messages: []watchdapi.Notification{}}, nil
					}
				},
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					telegramAttempts.Add(1)
					if !synchronized.Load() {
						telegramBeforeSync.Store(true)
					}
					if failGetMe {
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(w, `{"ok":false,"error_code":500,"description":"synthetic outage"}`)
						return
					}
					_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test","username":"testbot"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
			}))
			t.Cleanup(srv.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() {
				cancel()
				close(release)
			})
			b, err := New(ctx, testConfig(), testPaths(t), "v0.0.0", "v0.0.0-test", "deadbee", "2026-01-01",
				WithChatStore(store), withNotifications(api), withAPIBase(srv.URL))
			if b == nil || (err != nil) != failGetMe {
				t.Fatalf("New returned bot=%v error=%v, failGetMe=%v", b != nil, err, failGetMe)
			}
			receiverWait(t, pendingStarted, "receiver started by New")
			if telegramAttempts.Load() == 0 || telegramBeforeSync.Load() {
				t.Fatalf("Telegram attempts=%d, attempted before recipient sync=%v", telegramAttempts.Load(), telegramBeforeSync.Load())
			}
			if err := b.Connect(testConfig()); (err != nil) != failGetMe {
				t.Fatalf("Connect retry = %v, failGetMe=%v", err, failGetMe)
			}
			if pendingCalls.Load() != 1 {
				t.Fatalf("receiver poll starts = %d, want one despite Connect retry", pendingCalls.Load())
			}
		})
	}
}

// The store stands on the watchd side of the NotificationAPI boundary.
type queueNotificationAPI struct{ queue *notifications.Store }

func (a queueNotificationAPI) SetRecipients(ctx context.Context, recipients []watchdapi.Recipient) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.queue.ReplaceRecipients(recipients)
}
func (a queueNotificationAPI) Pending(ctx context.Context, cursor string) (watchdapi.NotificationPage, error) {
	if err := ctx.Err(); err != nil {
		return watchdapi.NotificationPage{}, err
	}
	return a.queue.Pending(cursor)
}
func (a queueNotificationAPI) Ack(ctx context.Context, chatID int64, id watchdapi.EventID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.queue.Ack(chatID, id)
}

func legacyReceiver(t *testing.T, sender telegram.MessageSender, withBob bool, now func() time.Time) (*Bot, *notifications.Store, *atomic.Bool) {
	t.Helper()
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	q, err := notifications.NewStore(filepath.Join(t.TempDir(), "watchd-notifications.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	b := receiverBot(t, queueNotificationAPI{q}, sender)
	if !withBob {
		b.auth = NewAuth([]string{"alice", "alice_renamed"})
	}
	live := &atomic.Bool{}
	live.Store(true)
	b.pathLive = live.Load
	if err := b.syncRecipients(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b, q, live
}

func legacyPublish(t *testing.T, q *notifications.Store, text string) watchdapi.EventID {
	t.Helper()
	id, err := q.Publish(text)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReceiver_LegacyWaitsForAPathToTelegram(t *testing.T) {
	transport := &receiverTelegram{}
	b, q, live := legacyReceiver(t, telegram.NewSender(transport), false, nil)
	live.Store(false)
	legacyPublish(t, q, "Xray outbound is dead")
	_ = receiverPoll(b)
	if n := len(transport.texts(100, true)); n != 0 {
		t.Fatalf("%d send attempts with no Telegram path, want none", n)
	}
	live.Store(true)
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	if got := transport.texts(100, false); !reflect.DeepEqual(got, []string{"Xray outbound is dead"}) {
		t.Fatalf("delivered %q after path recovery", got)
	}
}

func TestReceiver_LegacyKeepsAMessageWhoseSendFailed(t *testing.T) {
	var attempts atomic.Int64
	transport := &receiverTelegram{send: func(tgbotapi.MessageConfig) error {
		if attempts.Add(1) == 1 {
			return errors.New("synthetic stale Telegram path")
		}
		return nil
	}}
	b, q, _ := legacyReceiver(t, telegram.NewSender(transport), false, nil)
	legacyPublish(t, q, "Xray outbound is dead")
	_ = receiverPoll(b)
	if got := transport.texts(100, false); len(got) != 0 || q.Status().Pending != 1 {
		t.Fatalf("failed send lost/prematurely delivered its event: %q pending=%d", got, q.Status().Pending)
	}
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	if got := transport.texts(100, false); !reflect.DeepEqual(got, []string{"Xray outbound is dead"}) || attempts.Load() != 2 {
		t.Fatalf("retry delivered %q attempts=%d", got, attempts.Load())
	}
}

func TestReceiver_LegacyRetriesOnlyWhatTelegramDidNotRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kept bool
	}{
		{"no_path", errors.New("synthetic Telegram transport outage"), true},
		{"too_many_requests", &tgbotapi.Error{Code: 429, Message: "synthetic retry after 5"}, true},
		{"bad_gateway", &tgbotapi.Error{Code: 502, Message: "synthetic gateway refusal"}, true},
		{"blocked_by_user", &tgbotapi.Error{Code: 403, Message: "synthetic blocked user"}, false},
		{"bad_request", &tgbotapi.Error{Code: 400, Message: "synthetic chat not found"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int64
			transport := &receiverTelegram{send: func(tgbotapi.MessageConfig) error {
				if attempts.Add(1) == 1 {
					return tc.err
				}
				return nil
			}}
			b, q, _ := legacyReceiver(t, telegram.NewSender(transport), false, nil)
			legacyPublish(t, q, "Xray outbound is dead")
			_ = receiverPoll(b)
			if err := receiverPoll(b); err != nil {
				t.Fatal(err)
			}
			delivered := len(transport.texts(100, false)) == 1
			if delivered != tc.kept {
				t.Fatalf("delivered on retry=%v, want %v", delivered, tc.kept)
			}
			if n := attempts.Load(); tc.kept && n != 2 || !tc.kept && n != 1 {
				t.Fatalf("attempts=%d", n)
			}
		})
	}
}

func TestReceiver_LegacyNewMessageWaitsBehindAnOlderOne(t *testing.T) {
	var attempts atomic.Int64
	transport := &receiverTelegram{send: func(tgbotapi.MessageConfig) error {
		if attempts.Add(1) == 1 {
			return errors.New("synthetic Telegram outage")
		}
		return nil
	}}
	b, q, _ := legacyReceiver(t, telegram.NewSender(transport), false, nil)
	legacyPublish(t, q, "Xray outbound is dead")
	_ = receiverPoll(b)
	legacyPublish(t, q, "Xray moved to Beta / Germany-1")
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	want := []string{"Xray outbound is dead", "Xray moved to Beta / Germany-1"}
	if got := transport.texts(100, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %q, want %q", got, want)
	}
}

func TestReceiver_LegacyDelayedMinuteAndDayTimestamps(t *testing.T) {
	for _, tc := range []struct {
		name       string
		at, now    time.Time
		text, want string
	}{
		{"late_outage", time.Date(2026, 9, 25, 10, 46, 59, 0, time.Local), time.Date(2026, 9, 25, 10, 48, 5, 0, time.Local), "Xray outbound is dead", "(10:46, delayed) Xray outbound is dead"},
		{"fresh_followup", time.Date(2026, 9, 25, 10, 47, 49, 0, time.Local), time.Date(2026, 9, 25, 10, 48, 5, 0, time.Local), "Xray moved to Beta / Germany-1", "Xray moved to Beta / Germany-1"},
		{"previous_day", time.Date(2026, 9, 24, 23, 50, 0, 0, time.Local), time.Date(2026, 9, 25, 0, 20, 0, 0, time.Local), "Xray outbound is dead", "(Sep 24 23:50, delayed) Xray outbound is dead"},
		{"minute_boundary", time.Date(2026, 9, 25, 10, 46, 0, 0, time.Local), time.Date(2026, 9, 25, 10, 47, 0, 0, time.Local), "Xray outbound is dead", "(10:46, delayed) Xray outbound is dead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := notificationText(watchdapi.Notification{ChatID: 100, At: tc.at, Text: tc.text}, tc.now); got != tc.want {
				t.Fatalf("delayed text=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestReceiver_LegacyDelayedDeliveryUsesStoredTime(t *testing.T) {
	transport := &receiverTelegram{}
	at := time.Now().Add(-2 * time.Minute)
	clock := at
	b, q, live := legacyReceiver(t, telegram.NewSender(transport), false, func() time.Time { return clock })
	live.Store(false)
	legacyPublish(t, q, "Xray outbound is dead")
	_ = receiverPoll(b)
	if got := transport.texts(100, true); len(got) != 0 {
		t.Fatalf("attempted %q before path recovery", got)
	}
	clock = time.Now()
	legacyPublish(t, q, "Xray moved to Beta / Germany-1")
	page, err := q.Pending("")
	if err != nil || len(page.Messages) != 2 || !page.Messages[0].At.Equal(at) {
		t.Fatalf("queue lost the original event timestamp: page=%+v error=%v", page, err)
	}
	stamp := at.Format("15:04")
	if year, day := at.Year(), at.YearDay(); year != clock.Year() || day != clock.YearDay() {
		stamp = at.Format("Jan 2 15:04")
	}
	live.Store(true)
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	want := []string{"(" + stamp + ", delayed) Xray outbound is dead", "Xray moved to Beta / Germany-1"}
	if got := transport.texts(100, false); !reflect.DeepEqual(got, want) || q.Status().Pending != 0 {
		t.Fatalf("stored-time delivery=%q pending=%d, want %q and both acked", got, q.Status().Pending, want)
	}
}

func TestReceiver_LegacyKeepsTheLatestTwentyMessages(t *testing.T) {
	transport := &receiverTelegram{}
	b, q, live := legacyReceiver(t, telegram.NewSender(transport), false, nil)
	live.Store(false)
	texts := []string{"m01", "m02", "m03", "m04", "m05", "m06", "m07", "m08", "m09", "m10", "m11", "m12", "m13", "m14", "m15", "m16", "m17", "m18", "m19", "m20", "m21"}
	for _, text := range texts {
		legacyPublish(t, q, text)
	}
	_ = receiverPoll(b)
	if len(transport.texts(100, true)) != 0 {
		t.Fatal("long outage attempted Telegram delivery")
	}
	live.Store(true)
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	if got, want := transport.texts(100, false), texts[1:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %q, want latest twenty %q", got, want)
	}
}

func TestReceiver_LegacyDropsAMessageOlderThanTwelveHours(t *testing.T) {
	transport := &receiverTelegram{}
	now := time.Now()
	clock := now.Add(-12*time.Hour - time.Minute)
	b, q, live := legacyReceiver(t, telegram.NewSender(transport), false, func() time.Time { return clock })
	live.Store(false)
	legacyPublish(t, q, "Xray outbound is dead")
	clock = now
	legacyPublish(t, q, "LAN clients back on Xray")
	live.Store(true)
	if err := receiverPoll(b); err != nil {
		t.Fatal(err)
	}
	if got := transport.texts(100, false); !reflect.DeepEqual(got, []string{"LAN clients back on Xray"}) {
		t.Fatalf("12-hour expiry delivered %q", got)
	}
	at := time.Date(2026, 9, 25, 20, 0, 0, 0, time.Local)
	if got := notificationText(watchdapi.Notification{ChatID: 100, At: at, Text: "LAN clients back on Xray"}, time.Date(2026, 9, 25, 21, 1, 0, 0, time.Local)); got != "(20:00, delayed) LAN clients back on Xray" {
		t.Fatalf("retained delayed restore=%q", got)
	}
}

func TestReceiver_LegacyFailingChatDoesNotHoldUpAnother(t *testing.T) {
	transport := &receiverTelegram{send: func(message tgbotapi.MessageConfig) error {
		if message.ChatID == 100 {
			return errors.New("synthetic alice-only failure")
		}
		return nil
	}}
	b, q, _ := legacyReceiver(t, telegram.NewSender(transport), true, nil)
	legacyPublish(t, q, "Xray outbound is dead")
	_ = receiverPoll(b)
	if got := transport.texts(200, false); !reflect.DeepEqual(got, []string{"Xray outbound is dead"}) {
		t.Fatalf("independent chat delivered %q", got)
	}
	page, err := q.Pending("")
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ChatID != 100 || page.Messages[0].Text != "Xray outbound is dead" {
		t.Fatalf("alice's retry event was lost: page=%+v error=%v", page, err)
	}
}

func TestReceiver_LegacyPeriodicRetryDeliversOnceAPathIsBack(t *testing.T) {
	transport := &receiverTelegram{}
	b, q, live := legacyReceiver(t, telegram.NewSender(transport), false, nil)
	live.Store(false)
	legacyPublish(t, q, "Xray outbound is dead")
	_ = receiverPoll(b)
	if got := transport.texts(100, true); len(got) != 0 {
		t.Fatalf("sent %q without a path", got)
	}
	periodic, release, acked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var syncCalls atomic.Int64
	var firstAck, releaseOnce sync.Once
	releaseSync := func() { releaseOnce.Do(func() { close(release) }) }
	api := &receiverAPI{
		set: func(ctx context.Context, recipients []watchdapi.Recipient) error {
			if syncCalls.Add(1) == 2 {
				close(periodic)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return q.ReplaceRecipients(recipients)
		},
		pending: func(_ context.Context, cursor string) (watchdapi.NotificationPage, error) {
			return q.Pending(cursor)
		},
		ack: func(_ context.Context, chatID int64, id watchdapi.EventID) error {
			err := q.Ack(chatID, id)
			firstAck.Do(func() { close(acked) })
			return err
		},
	}
	b.notificationAPI = api
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.receiveNotifications(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		releaseSync()
		receiverWait(t, done, "periodic receiver shutdown")
	})
	select {
	case <-periodic:
	case <-time.After(12 * time.Second):
		t.Fatal("receiver did not retry/sync on its real 10-second schedule")
	}
	if got := transport.texts(100, true); len(got) != 0 {
		t.Fatalf("receiver sent %q before a path recovered", got)
	}
	live.Store(true)
	releaseSync()
	receiverWait(t, acked, "periodic delivery after path recovery")
	if got := transport.texts(100, false); !reflect.DeepEqual(got, []string{"Xray outbound is dead"}) {
		t.Fatalf("periodic recovery delivered %q", got)
	}
	if q.Status().Pending != 0 {
		t.Fatal("periodic delivery left the acknowledged event pending")
	}
	_, cursors, _ := api.snapshot()
	if !reflect.DeepEqual(cursors, []string{"", ""}) {
		t.Fatalf("periodic retry did not restart from an empty cursor: %q", cursors)
	}
}
