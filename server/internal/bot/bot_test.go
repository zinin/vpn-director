package bot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/chatstore"
	"github.com/zinin/vpn-director/server/internal/config"
	"github.com/zinin/vpn-director/server/internal/devmode"
	"github.com/zinin/vpn-director/server/internal/paths"
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

func TestNew_DevModeDoesNotStartWatch(t *testing.T) {
	srv := newAPIServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b, err := New(ctx, testConfig(), testPaths(t), "v0.0.0", "v0.0.0-test", "deadbee", "2026-01-01",
		WithDevMode(devmode.NewExecutor()), withAPIBase(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if b.subWatch != nil {
		t.Fatal("dev mode must not start the subscription watch")
	}
}

func TestNew_WatchStartsWhenGetMeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":500,"description":"down"}`)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b, err := New(ctx, testConfig(), testPaths(t), "v0.0.0", "v0.0.0-test", "deadbee", "2026-01-01",
		withAPIBase(srv.URL))
	if err == nil {
		t.Fatal("getMe must fail")
	}
	if b == nil || b.subWatch == nil {
		t.Fatal("subscription watch must start even when Telegram authorization fails")
	}
}

func TestNew_ProductionUsesPathClientAndManager(t *testing.T) {
	srv := newAPIServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b, err := New(ctx, testConfig(), testPaths(t), "v0.0.0", "v0.0.0-test", "deadbee", "2026-01-01",
		withAPIBase(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if b.pathManager == nil {
		t.Fatal("production must build a PathManager")
	}
	if got := b.pathManager.Current().String(); got != "direct" {
		t.Fatalf("path %q; the local server answers the probe, so direct is live", got)
	}
	c, ok := b.api.Client.(*http.Client)
	if !ok {
		t.Fatalf("client is %T", b.api.Client)
	}
	if _, isPath := c.Transport.(*pathTransport); !isPath {
		t.Fatalf("transport is %T; production must dial through a pathTransport", c.Transport)
	}
	deadline := time.Now().Add(time.Second)
	for !b.pathManager.running {
		if time.Now().After(deadline) {
			t.Fatal("path monitor must start before getMe so a blackhole can fail over")
		}
		time.Sleep(time.Millisecond)
	}
	if b.subWatch == nil {
		t.Fatal("production must start the subscription watch")
	}
}

type sentPlain struct {
	chatID int64
	text   string
}

// recordingSender keeps every SendPlain; the rest of MessageSender is unused.
type recordingSender struct {
	plain []sentPlain
}

func (s *recordingSender) Send(int64, string) error { return nil }
func (s *recordingSender) SendPlain(chatID int64, text string) error {
	s.plain = append(s.plain, sentPlain{chatID: chatID, text: text})
	return nil
}
func (s *recordingSender) SendLongPlain(int64, string) error { return nil }
func (s *recordingSender) SendWithKeyboard(int64, string, tgbotapi.InlineKeyboardMarkup) error {
	return nil
}
func (s *recordingSender) SendCodeBlock(int64, string, string) error { return nil }
func (s *recordingSender) EditMessage(int64, int, string, tgbotapi.InlineKeyboardMarkup) error {
	return nil
}
func (s *recordingSender) AckCallback(string) error { return nil }

func TestNotifyActiveChats_AuthorizedOncePerChat(t *testing.T) {
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
	sender := &recordingSender{}
	b := &Bot{auth: NewAuth([]string{"alice", "alice_renamed"}), sender: sender, chatStore: store}

	b.notifyActiveChats("Xray outbound is down")

	want := []sentPlain{{chatID: 100, text: "Xray outbound is down"}}
	if !reflect.DeepEqual(sender.plain, want) {
		t.Fatalf("sent %+v, want %+v", sender.plain, want)
	}
}
