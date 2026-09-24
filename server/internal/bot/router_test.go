// internal/bot/router_test.go
package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Mock handlers for testing

type mockStatusHandler struct {
	statusCalled  bool
	restartCalled bool
	stopCalled    bool
}

func (m *mockStatusHandler) HandleStatus(msg *tgbotapi.Message)  { m.statusCalled = true }
func (m *mockStatusHandler) HandleRestart(msg *tgbotapi.Message) { m.restartCalled = true }
func (m *mockStatusHandler) HandleStop(msg *tgbotapi.Message)    { m.stopCalled = true }

type mockServersHandler struct {
	serversCalled  bool
	callbackCalled bool
}

func (m *mockServersHandler) HandleServers(msg *tgbotapi.Message)       { m.serversCalled = true }
func (m *mockServersHandler) HandleCallback(cb *tgbotapi.CallbackQuery) { m.callbackCalled = true }

type mockImportHandler struct {
	importCalled bool
}

func (m *mockImportHandler) HandleImport(msg *tgbotapi.Message) { m.importCalled = true }

type mockMiscHandler struct {
	startCalled   bool
	ipCalled      bool
	versionCalled bool
	logsCalled    bool
}

func (m *mockMiscHandler) HandleStart(msg *tgbotapi.Message)   { m.startCalled = true }
func (m *mockMiscHandler) HandleIP(msg *tgbotapi.Message)      { m.ipCalled = true }
func (m *mockMiscHandler) HandleVersion(msg *tgbotapi.Message) { m.versionCalled = true }
func (m *mockMiscHandler) HandleLogs(msg *tgbotapi.Message)    { m.logsCalled = true }

type mockWizardHandler struct {
	startCalled      bool
	startChatID      int64
	callbackCalled   bool
	textCalled       bool
	clearStateCalled bool
}

func (m *mockWizardHandler) Start(chatID int64)                        { m.startCalled = true; m.startChatID = chatID }
func (m *mockWizardHandler) ClearState(chatID int64)                   { m.clearStateCalled = true }
func (m *mockWizardHandler) HandleCallback(cb *tgbotapi.CallbackQuery) { m.callbackCalled = true }
func (m *mockWizardHandler) HandleTextInput(msg *tgbotapi.Message)     { m.textCalled = true }

type mockXrayHandler struct {
	xrayCalled     bool
	callbackCalled bool
}

func (m *mockXrayHandler) HandleXray(msg *tgbotapi.Message)          { m.xrayCalled = true }
func (m *mockXrayHandler) HandleCallback(cb *tgbotapi.CallbackQuery) { m.callbackCalled = true }

type mockExcludeHandler struct {
	excludeCalled    bool
	callbackCalled   bool
	textCalled       bool
	clearStateCalled bool
}

func (m *mockExcludeHandler) HandleExclude(msg *tgbotapi.Message)       { m.excludeCalled = true }
func (m *mockExcludeHandler) ClearState(chatID int64)                   { m.clearStateCalled = true }
func (m *mockExcludeHandler) HandleCallback(cb *tgbotapi.CallbackQuery) { m.callbackCalled = true }
func (m *mockExcludeHandler) HandleTextInput(msg *tgbotapi.Message)     { m.textCalled = true }

type mockClientsHandler struct {
	clientsCalled    bool
	callbackCalled   bool
	textInputCalled  bool
	clearStateCalled bool
}

func (m *mockClientsHandler) HandleClients(msg *tgbotapi.Message)       { m.clientsCalled = true }
func (m *mockClientsHandler) HandleCallback(cb *tgbotapi.CallbackQuery) { m.callbackCalled = true }
func (m *mockClientsHandler) HandleTextInput(msg *tgbotapi.Message)     { m.textInputCalled = true }
func (m *mockClientsHandler) ClearState(chatID int64)                   { m.clearStateCalled = true }

// Helper to create a message with command entity
func msgWithCommand(text string) *tgbotapi.Message {
	cmdLen := len(text)
	if idx := indexOf(text, ' '); idx > 0 {
		cmdLen = idx
	}
	return &tgbotapi.Message{
		Text:     text,
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: cmdLen}},
		Chat:     &tgbotapi.Chat{ID: 123},
	}
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// Tests for RouteMessage

func TestRouter_RouteMessage_Status(t *testing.T) {
	h := &mockStatusHandler{}
	router := &Router{status: h}

	router.RouteMessage(msgWithCommand("/status"))

	if !h.statusCalled {
		t.Error("expected HandleStatus to be called")
	}
}

func TestRouter_RouteMessage_Restart(t *testing.T) {
	h := &mockStatusHandler{}
	router := &Router{status: h}

	router.RouteMessage(msgWithCommand("/restart"))

	if !h.restartCalled {
		t.Error("expected HandleRestart to be called")
	}
}

func TestRouter_RouteMessage_Stop(t *testing.T) {
	h := &mockStatusHandler{}
	router := &Router{status: h}

	router.RouteMessage(msgWithCommand("/stop"))

	if !h.stopCalled {
		t.Error("expected HandleStop to be called")
	}
}

func TestRouter_RouteMessage_Servers(t *testing.T) {
	h := &mockServersHandler{}
	router := &Router{servers: h}

	router.RouteMessage(msgWithCommand("/servers"))

	if !h.serversCalled {
		t.Error("expected HandleServers to be called")
	}
}

func TestRouter_RouteMessage_Import(t *testing.T) {
	h := &mockImportHandler{}
	router := &Router{import_: h}

	router.RouteMessage(msgWithCommand("/import https://example.com"))

	if !h.importCalled {
		t.Error("expected HandleImport to be called")
	}
}

func TestRouter_RouteMessage_Start(t *testing.T) {
	h := &mockMiscHandler{}
	router := &Router{misc: h}

	router.RouteMessage(msgWithCommand("/start"))

	if !h.startCalled {
		t.Error("expected HandleStart to be called")
	}
}

func TestRouter_RouteMessage_IP(t *testing.T) {
	h := &mockMiscHandler{}
	router := &Router{misc: h}

	router.RouteMessage(msgWithCommand("/ip"))

	if !h.ipCalled {
		t.Error("expected HandleIP to be called")
	}
}

func TestRouter_RouteMessage_Version(t *testing.T) {
	h := &mockMiscHandler{}
	router := &Router{misc: h}

	router.RouteMessage(msgWithCommand("/version"))

	if !h.versionCalled {
		t.Error("expected HandleVersion to be called")
	}
}

func TestRouter_RouteMessage_Logs(t *testing.T) {
	h := &mockMiscHandler{}
	router := &Router{misc: h}

	router.RouteMessage(msgWithCommand("/logs bot 50"))

	if !h.logsCalled {
		t.Error("expected HandleLogs to be called")
	}
}

func TestRouter_RouteMessage_Configure(t *testing.T) {
	h := &mockWizardHandler{}
	eh := &mockExcludeHandler{}
	ch := &mockClientsHandler{}
	router := &Router{wizard: h, exclude: eh, clients: ch}

	msg := msgWithCommand("/configure")
	router.RouteMessage(msg)

	if !h.startCalled {
		t.Error("expected wizard.Start to be called")
	}
	if h.startChatID != 123 {
		t.Errorf("expected chatID 123, got %d", h.startChatID)
	}
}

func TestRouter_RouteMessage_UnknownCommand_RoutesToWizardText(t *testing.T) {
	h := &mockWizardHandler{}
	eh := &mockExcludeHandler{}
	ch := &mockClientsHandler{}
	router := &Router{wizard: h, exclude: eh, clients: ch}

	// Plain text (no command entity) - should go to clients, exclude, and wizard text handlers
	msg := &tgbotapi.Message{
		Text: "192.168.1.100",
		Chat: &tgbotapi.Chat{ID: 123},
	}
	router.RouteMessage(msg)

	if !ch.textInputCalled {
		t.Error("expected clients.HandleTextInput to be called for plain text")
	}
	if !h.textCalled {
		t.Error("expected wizard.HandleTextInput to be called for plain text")
	}
	if !eh.textCalled {
		t.Error("expected exclude.HandleTextInput to be called for plain text")
	}
}

// Tests for RouteCallback

func TestRouter_RouteCallback_Servers(t *testing.T) {
	h := &mockServersHandler{}
	router := &Router{servers: h}

	cb := &tgbotapi.CallbackQuery{
		Data:    "servers:page:1",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}
	router.RouteCallback(cb)

	if !h.callbackCalled {
		t.Error("expected HandleCallback to be called for servers:page:*")
	}
}

func TestRouter_RouteCallback_ServersNoop(t *testing.T) {
	h := &mockServersHandler{}
	router := &Router{servers: h}

	cb := &tgbotapi.CallbackQuery{
		Data:    "servers:noop",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}
	router.RouteCallback(cb)

	if !h.callbackCalled {
		t.Error("expected HandleCallback to be called for servers:noop")
	}
}

func TestRouter_RouteCallback_Wizard(t *testing.T) {
	h := &mockWizardHandler{}
	router := &Router{wizard: h}

	cb := &tgbotapi.CallbackQuery{
		Data:    "server:1",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}
	router.RouteCallback(cb)

	if !h.callbackCalled {
		t.Error("expected wizard.HandleCallback to be called")
	}
}

func TestRouter_RouteCallback_WizardApply(t *testing.T) {
	h := &mockWizardHandler{}
	router := &Router{wizard: h}

	cb := &tgbotapi.CallbackQuery{
		Data:    "apply",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}
	router.RouteCallback(cb)

	if !h.callbackCalled {
		t.Error("expected wizard.HandleCallback to be called for apply")
	}
}

func TestRouter_RouteCallback_WizardCancel(t *testing.T) {
	h := &mockWizardHandler{}
	router := &Router{wizard: h}

	cb := &tgbotapi.CallbackQuery{
		Data:    "cancel",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}
	router.RouteCallback(cb)

	if !h.callbackCalled {
		t.Error("expected wizard.HandleCallback to be called for cancel")
	}
}

func TestRouter_RouteMessage_Xray(t *testing.T) {
	h := &mockXrayHandler{}
	router := &Router{xray: h}

	router.RouteMessage(msgWithCommand("/xray"))

	if !h.xrayCalled {
		t.Error("expected HandleXray to be called")
	}
}

func TestRouter_RouteCallback_Xray(t *testing.T) {
	h := &mockXrayHandler{}
	router := &Router{xray: h}

	cb := &tgbotapi.CallbackQuery{
		Data:    "xray:select:0",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}
	router.RouteCallback(cb)

	if !h.callbackCalled {
		t.Error("expected HandleCallback to be called for xray:select:*")
	}
}

func TestRouter_RouteMessage_Exclude(t *testing.T) {
	h := &mockExcludeHandler{}
	wh := &mockWizardHandler{}
	ch := &mockClientsHandler{}
	router := &Router{exclude: h, wizard: wh, clients: ch}

	router.RouteMessage(msgWithCommand("/exclude"))

	if !h.excludeCalled {
		t.Error("expected HandleExclude to be called")
	}
}

func TestRouter_RouteCallback_WizardExcludeIPs(t *testing.T) {
	h := &mockWizardHandler{}
	router := &Router{wizard: h}

	cb := &tgbotapi.CallbackQuery{
		Data:    "wexclip:add",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}
	router.RouteCallback(cb)

	if !h.callbackCalled {
		t.Error("expected wizard.HandleCallback to be called for wexclip:*")
	}
}

func TestRouter_RouteCallback_Exclude(t *testing.T) {
	h := &mockExcludeHandler{}
	router := &Router{exclude: h}

	cb := &tgbotapi.CallbackQuery{
		Data:    "exclip:add",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}
	router.RouteCallback(cb)

	if !h.callbackCalled {
		t.Error("expected HandleCallback to be called for exclip:*")
	}
}

func TestRouter_RouteMessage_Clients(t *testing.T) {
	h := &mockClientsHandler{}
	eh := &mockExcludeHandler{}
	wh := &mockWizardHandler{}
	router := &Router{clients: h, exclude: eh, wizard: wh}

	router.RouteMessage(msgWithCommand("/clients"))

	if !h.clientsCalled {
		t.Error("expected HandleClients to be called")
	}
}

func TestRouter_RouteCallback_Clients(t *testing.T) {
	h := &mockClientsHandler{}
	router := &Router{clients: h}

	cb := &tgbotapi.CallbackQuery{
		Data:    "clients:pause:192.168.50.10",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}
	router.RouteCallback(cb)

	if !h.callbackCalled {
		t.Error("expected HandleCallback to be called")
	}
}

func TestRouter_RouteMessage_Clients_ClearsOtherStates(t *testing.T) {
	clients := &mockClientsHandler{}
	exclude := &mockExcludeHandler{}
	wizard := &mockWizardHandler{}
	router := &Router{clients: clients, exclude: exclude, wizard: wizard}

	router.RouteMessage(msgWithCommand("/clients"))

	if !exclude.clearStateCalled {
		t.Error("expected exclude ClearState to be called")
	}
	if !wizard.clearStateCalled {
		t.Error("expected wizard ClearState to be called")
	}
}

type mockSubsHandler struct {
	subsCalled, cancelCalled, callbackCalled, textCalled bool
	cleared                                              int
	takes                                                bool // what HandleTextInput answers
}

func (m *mockSubsHandler) HandleSubs(*tgbotapi.Message)           { m.subsCalled = true }
func (m *mockSubsHandler) HandleCancel(*tgbotapi.Message)         { m.cancelCalled = true }
func (m *mockSubsHandler) HandleCallback(*tgbotapi.CallbackQuery) { m.callbackCalled = true }
func (m *mockSubsHandler) HandleTextInput(*tgbotapi.Message) bool {
	m.textCalled = true
	return m.takes
}
func (m *mockSubsHandler) ClearState(int64) { m.cleared++ }

func TestRouter_RouteMessage_Subs(t *testing.T) {
	s := &mockSubsHandler{}
	router := &Router{subs: s}

	router.RouteMessage(msgWithCommand("/subs"))

	if !s.subsCalled {
		t.Error("expected HandleSubs to be called")
	}
}

func TestRouter_RouteMessage_Cancel(t *testing.T) {
	s := &mockSubsHandler{}
	router := &Router{subs: s}

	router.RouteMessage(msgWithCommand("/cancel"))

	if !s.cancelCalled {
		t.Error("expected HandleCancel to be called")
	}
}

// Any command ends a rename that waits for its name.
func TestRouter_AnyCommandEndsAPendingRename(t *testing.T) {
	s := &mockSubsHandler{}
	router := &Router{subs: s, status: &mockStatusHandler{}}

	router.RouteMessage(msgWithCommand("/status"))

	if s.cleared != 1 {
		t.Errorf("ClearState called %d times, want 1", s.cleared)
	}
}

// A rename waiting for its name takes the text before the wizard does.
func TestRouter_APendingRenameTakesTheTextFirst(t *testing.T) {
	s := &mockSubsHandler{takes: true}
	w, e, c := &mockWizardHandler{}, &mockExcludeHandler{}, &mockClientsHandler{}
	router := &Router{subs: s, wizard: w, exclude: e, clients: c}

	router.RouteMessage(&tgbotapi.Message{Text: "Main", Chat: &tgbotapi.Chat{ID: 123}})

	if !s.textCalled || w.textCalled || e.textCalled || c.textInputCalled {
		t.Errorf("subs %v, wizard %v, exclude %v, clients %v", s.textCalled, w.textCalled, e.textCalled, c.textInputCalled)
	}
}

// A button outside /subs ends a rename that waits for its name: clients:add
// asks for an address, and the address must not rename the subscription.
func TestRouter_AButtonOutsideSubsEndsARename(t *testing.T) {
	s, c := &mockSubsHandler{}, &mockClientsHandler{}
	router := &Router{subs: s, clients: c}

	router.RouteCallback(&tgbotapi.CallbackQuery{
		Data:    "clients:add",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	})

	if s.cleared != 1 {
		t.Errorf("ClearState called %d times, want 1", s.cleared)
	}
	if !c.callbackCalled {
		t.Error("expected clients.HandleCallback to be called")
	}
}

// A button of /subs itself leaves the rename to the subs handler.
func TestRouter_ASubsButtonKeepsTheRename(t *testing.T) {
	s := &mockSubsHandler{}
	router := &Router{subs: s}

	router.RouteCallback(&tgbotapi.CallbackQuery{
		Data:    "subs:r:0a1b2c3d",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	})

	if s.cleared != 0 {
		t.Errorf("ClearState called %d times, want 0", s.cleared)
	}
	if !s.callbackCalled {
		t.Error("expected subs.HandleCallback to be called")
	}
}

func TestRouter_TextWithoutARenameGoesToTheWizard(t *testing.T) {
	s := &mockSubsHandler{}
	w, e, c := &mockWizardHandler{}, &mockExcludeHandler{}, &mockClientsHandler{}
	router := &Router{subs: s, wizard: w, exclude: e, clients: c}

	router.RouteMessage(&tgbotapi.Message{Text: "192.168.1.100", Chat: &tgbotapi.Chat{ID: 123}})

	if !w.textCalled {
		t.Error("expected wizard.HandleTextInput to be called")
	}
}

func TestRouter_RouteCallback_Subs(t *testing.T) {
	s := &mockSubsHandler{}
	router := &Router{subs: s, wizard: &mockWizardHandler{}}

	router.RouteCallback(&tgbotapi.CallbackQuery{Data: "subs:r:0a1b2c3d"})

	if !s.callbackCalled {
		t.Error("expected subs.HandleCallback to be called")
	}
}
