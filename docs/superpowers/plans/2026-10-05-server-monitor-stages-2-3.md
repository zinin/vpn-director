# Server Monitor Stages 2+3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Перенести автоматический failover в `vpn-director-watchd`, сохранить Telegram-уведомления между перезапусками и использовать свежие результаты монитора для безопасного раннего переключения.

**Architecture:** Watchd владеет `subwatch`, монитором подключений и файловой очередью; бот получает события через Unix API и подтверждает доставку. `internal/netpath` содержит общий транспорт, `internal/service` — загрузку подписок. Быстрый путь повторно проверяет результаты монитора, совместимость бота и владение конфигурацией перед публикацией каждой автоматической записи.

**Tech Stack:** Go и существующие стандартные HTTP/Unix-socket интерфейсы, Xray, Vue 3/TypeScript/Vite, Bash/BusyBox, Bats; существующий config `flock` и atomic rename.

**Spec:** `docs/superpowers/specs/2026-10-05-server-monitor-stages-2-3-design.md`

## Global Constraints

- Работа выполняется в текущем checkout, в `feature/server-monitor`, для существующего PR #63. Последовательность задач — 1 → 16; новая ветка/worktree не создаётся.
- Этот документ описывает реализацию. Её начало требует проверки плана пользователем и выбора метода исполнения.
- Поддерживаются Asuswrt-Merlin и KeeneticOS; firmware facts получают через существующий platform contract. Пробный процесс сохраняет имя `vpn-director-probe`, root-only socket — mode `0600`.
- Основной HTTPS SOCKS probe — каждые 30 секунд; `monitor.interval` сохраняется, default 1 минута. За быструю попытку проверяются все активные ключи и до 3 других различных подключений; общий deadline дополнительных проверок — 30 секунд, worker pool монитора остаётся общим.
- Legacy подтверждение отказа: 1 минута при доказанной TCP-недоступности, 3 минуты иначе. Сохраняются `OwnFirst=3`, `PerAddress`, preferred return 5 минут, retry 10–30 минут, hold 30 минут, максимум 4 неудачных возврата.
- `/stop`, повторный ручной выбор того же сервера через `seq`, subscription deletion/refresh, readiness и make-before-break проверяются после ожиданий и под config lock. Автоматический restart: `vpn-director.sh --unless-stopped restart xray-process`.
- `monitor.enabled=false` отключает монитор/prober и сохраняет legacy failover. Завершение watchd сохраняет основной Xray и текущие маршруты; завершение бота сохраняет автоматику watchd.
- Файл очереди — `<data_dir>/watchd-notifications.json`, `0600`, staged write + file sync + rename + directory sync. Единственный writer — watchd; повреждённый оригинал сохраняется для диагностики.
- Pending cap — 20 на чат; recent history — 20 событий; TTL — 12 часов; delayed timestamp — от 1 минуты; bot poll и dirty-save retry — каждые 10 секунд; каждый IPC request — максимум 2 секунды.
- Новые POST bodies ограничены 1 MiB, oversize — 413; ответы клиента — 16 MiB; pending page — до 100 сообщений, cursor — до 256 символов. Текущие monitor DTO, HTTP statuses и `watchdapi.API` сохраняются.
- Fixture данные синтетические: documentation IP ranges и `example.*`. В логах/API/уведомлениях отсутствуют Telegram token, subscription URLs, server credentials и необработанные provider errors. UI и commit text — English; бот сохраняет существующие языковые соглашения.
- Все build/test/lint/type-check команды ниже исполняет `build-forge:build-runner`, строго один runner одновременно. Полная Bats suite запускается одним процессом. Dev servers запускает контроллер как принадлежащие задаче background commands.
- Сохраняются `.claude/settings.local.json`, session-transfer файлы, чужие prompts, `review.diff`, `test_exit.sh`, ignored/dev data. Stage содержит только точные пути текущей задачи; scratch-файлы находятся в `/tmp` и удаляются после использования.
- Для каждого Commit шага использовать указанные subject/body: `printf '%s\n\n%s\n' 'subject' 'body' > /tmp/vpd-stage23-commit.txt`, затем `git commit -F /tmp/vpd-stage23-commit.txt` и удалить файл. Body — один абзац, trailers отсутствуют.
- Перед публикацией реализации получить разрешение на удаление собственных planning документов и push; убрать spec/plan из net diff к `master`, сохранив историю. Обновление PR, Codex review и аппаратные изменения выполняются в пределах отдельного разрешения пользователя.

## Review Focus

1. Бинарник бота заменён, старый процесс продолжает работать: проверка `/proc/PID/exe` удерживает новую автоматику в `incompatible`; inode установленного файла сам по себе совместимость не доказывает. Тесты задач 6 и 8.
2. `/stop`, ручной повторный Select либо новый outbound под тем же именем появились во время probe, ожидания lock или Xray validation: прежняя автоматическая операция сохраняет текущую конфигурацию и прекращается. Тесты задач 6, 10 и 11.
3. Telegram уже принял сообщение, durable ack ещё не сохранён: текущий бот повторяет ack по устойчивому ID; ошибки одного чата сохраняют доставку другим. Тесты задач 3–5.
4. Очередь меняется между страницами или ответ приходит после нового запроса/unmount: следующий проход и generation guards сохраняют доставку и актуальное отображение. Тесты задач 4, 5 и 14.
5. WAN/prober outage, пустая подписка либо refresh с неизвестными ключами: здоровье подписки остаётся неопределённым, предыдущая отметка сохраняется и ложное событие смерти отсутствует. Тесты задач 9 и 12.

---

## File Structure and Boundaries

| Файлы | Ответственность | Задачи |
|---|---|---|
| `server/internal/netpath/path.go`, `server/internal/netpath/dial.go`, `server/internal/netpath/control_linux.go`, `server/internal/netpath/control_other.go`, `server/internal/netpath/reach.go`, `server/internal/netpath/readiness.go` | Общие пути, IPv4/bound DNS, socket control, TCP reach и чтение readiness paths | 1 |
| `server/internal/service/subfetch.go` | Единый WAN→tunnel fetch/decode/resolve | 2 |
| `server/internal/notifications/store.go`, `server/internal/notifications/persist.go`, `server/internal/notifications/recipients.go`, `server/internal/notifications/subscription_health.go` | Очередь, атомарное состояние, получатели и переходы здоровья подписок | 3, 12 |
| `server/internal/watchdapi/watch_types.go`, `server/internal/watchdapi/notifications_server.go`, `server/internal/watchdapi/notifications_client.go` | Раздельные watch/notification контракты поверх существующего socket | 3, 4 |
| `server/internal/bot/notifications.go`, `server/internal/bot/notification_text.go` | Получение, актуальная авторизация, отправка и повтор ack | 5 |
| `server/internal/watchcompat/capabilities.go`, `server/internal/watchcompat/gate.go`, `server/internal/watchcompat/proc_linux.go`, `server/internal/watchcompat/proc_other.go` | Read-only capability JSON, installed/running executable gate | 6 |
| `server/internal/subwatch/mutation.go`, `server/internal/subwatch/recovery.go`, `server/internal/subwatch/status.go`, `server/internal/subwatch/fast.go`, `server/internal/subwatch/health_order.go` | Mutation gate, restart recovery, watch snapshot, ранний переход и health-aware walk | 6–11 |
| `server/internal/service/activeserver.go`, `server/internal/service/xray.go`, `server/internal/service/context.go`; `server/internal/vpnconfig/pending_restore.go` | Проверка перед live rename, scoped services и durable restore intent | 6–8 |
| `server/cmd/watchd/runtime.go`, `server/cmd/watchd/watch.go`, `server/cmd/watchd/health.go` | Владение instance, DI и совместный жизненный цикл компонентов | 8, 12 |
| `server/internal/webapi/handler_watch.go`, `server/internal/handler/watch.go` | Авторизованное чтение состояния и `/status` | 13 |
| `web/src/components/WatchStatus.vue`, `web/src/watch.ts` | Единое представление состояния watch на Status и Servers | 14 |
| `install.sh`, init/updater tests, `CLAUDE.md`, `.claude/rules/watchd.md`, `.claude/rules/telegram-bot.md`, `.claude/rules/webui.md` | Установка, mixed-version recovery и эксплуатационные контракты | 15 |

Публичные интерфейсы определяются в owning задаче ниже. Test шаги задают имена тестов и проверяемые выражения; counters и входные fixtures создаются локально в этих тестах через существующие fake services и `t.TempDir()`. Новые production helpers и типы сверх Interfaces блоков добавляются только при необходимости реализации.

### Task 1: Выделить общий сетевой транспорт

**Files:** Create `server/internal/netpath/path.go`, `server/internal/netpath/dial.go`, `server/internal/netpath/control_linux.go`, `server/internal/netpath/control_other.go`, `server/internal/netpath/reach.go`, `server/internal/netpath/readiness.go`, `server/internal/netpath/path_test.go`, `server/internal/netpath/dial_test.go`, `server/internal/netpath/reach_test.go`, `server/internal/netpath/readiness_test.go`; Modify `server/internal/bot/probe_test.go`, `server/internal/bot/path.go`, `server/internal/bot/path_test.go`, `server/internal/bot/pathmanager.go`, `server/internal/bot/pathmanager_test.go`, `server/internal/bot/probe.go`, `server/internal/bot/transport.go`, `server/internal/bot/transport_test.go`, `server/internal/bot/subfetch.go`, `server/internal/bot/subfetch_test.go`, `server/internal/bot/bot.go`, `server/internal/bot/bot_test.go`; Move содержимое общих helpers/tests из `server/internal/bot/reach.go`, `server/internal/bot/reach_test.go`, `server/internal/bot/transport_linux.go`, `server/internal/bot/transport_other.go` в новые файлы, затем удалить эти четыре перенесённых файла.

**Interfaces:** Consumes `vpnconfig.TDExits`, `FailoverTDExit`, `XrayInboundPorts` и `ssrf.IsPrivateOrReserved`. Produces:
```go
// package netpath
// Kind values: KindNone, KindDirect, KindSOCKS, KindTunnel (same numeric order).
type Kind int
type Path struct { Kind Kind; ID, Iface string; SOCKSPort int; Mark uint32 }
func (p Path) String() string
func (p Path) Same(q Path) bool
func (p Path) ParamsEqual(q Path) bool
func SOCKSPort(cfg *vpnconfig.VPNDirectorConfig) int
func LoadTunnelIndexes(path string) map[string]int
func Candidates(cfg *vpnconfig.VPNDirectorConfig, plat vpnconfig.PlatformInfo, socksUp bool, indexes map[string]int) []Path
func TunnelPath(cfg *vpnconfig.VPNDirectorConfig, plat vpnconfig.PlatformInfo, id, tablesPath string) Path
func DialPath(ctx context.Context, p Path, network, addr string) (net.Conn, error)
func LookupIPv4(ctx context.Context, p Path, host string) ([]net.IP, error)
func ReachTCP4(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, int) bool
type Readiness struct { TablesPath, FailoverPath, TPROXYPath, StoppedPath string }
func (r Readiness) FallbackReady(id string) bool
func (r Readiness) TPROXYReady() bool
func (r Readiness) Stopped() bool
// package bot: keep PathSource and alias Path = netpath.Path.
func newPathClientWith(src PathSource, dial func(context.Context, Path, string, string) (net.Conn, error)) *http.Client
func probePathWith(ctx context.Context, apiBase, token string, p Path, dial func(context.Context, Path, string, string) (net.Conn, error)) error
```
- [ ] **Test:** Перенести assertions общих transport/path/reach tests; добавить `TestReadiness_UsesOnlyProvidedPaths`: synthetic marker даёт `ready == true`, отсутствие marker — `false`. `TestDial_TunnelDNSAndTCPShareMark`: оба control получают одинаковые `Iface` и `Mark`; first DNS timeout оставляет попытку второго DNS. Bot `Path` становится alias `netpath.Path`; Telegram `selectPath`, `getMe`, path retirement/long-poll tests остаются в боте.
- [ ] **RED:** `cd server && go test ./internal/netpath ./internal/bot -count=1` — новый пакет/API отсутствуют.
- [ ] **Implement:** Вынести dialer и его test injection seams без изменения 30s dial timeout, 2s DNS timeout/server и распределения deadline по IPv4 адресам. Экспортировать поля `Path` и адаптировать bot callers/tests; `NewPathClient` использует общий dialer, сохраняя Telegram failure reporting и keep-alive retirement. `newPathClientWith` и `probePathWith` принимают dial function; перенести test injection с изменяемого package-global dialer на эти seams, сохранив 8s Telegram probe deadline. Пакет `netpath` импортирует нижние зависимости и получает пути параметрами.
- [ ] **GREEN:** Та же команда и `cd server && go test -race ./internal/netpath ./internal/bot -count=1` — PASS; существующие TLS, cancel, SO_MARK/SO_BINDTODEVICE и long-poll assertions сохранены.
- [ ] **Commit:** Stage точные Files задачи, включая удаления перенесённых файлов. Subject: `refactor(netpath): share router network paths`; body: `Extract router dialing and readiness helpers while preserving Telegram transport behavior.`

### Task 2: Сделать fetch подписок общим для бота и watchd

**Files:** Create `server/internal/service/subfetch.go`, `server/internal/service/subfetch_test.go`; Modify `server/internal/bot/subfetch.go`, `server/internal/bot/subfetch_test.go`, `server/internal/bot/bot.go`. Общие fetch tests переносятся в service; файлы bot wrappers удаляются, когда последние callers/tests используют общий сервис.

**Interfaces:** Consumes Task 1 и существующие `service.ConfigStore`, `VPNDirector`, `DownloadError`, `ErrResolutionCutShort`. Produces:
```go
// package service
type SubscriptionFetcher struct { Store ConfigStore; VPN VPNDirector; TablesPath string }
func (f SubscriptionFetcher) Fetch(ctx context.Context, rawURL string) ([]vpnconfig.Server, error)
```
- [ ] **Test:** Перенести существующие `TestFetchServers_*`, `TestLazyTunnel_*`, body-cap, redirect и private-peer tests с исходными assertions. Добавить `TestSubscriptionFetcher_SharedDeadlinePublishesNoPartialList`: cancellation в середине resolution даёт `servers == nil`; deadline даёт `errors.Is(err, ErrResolutionCutShort) == true`; platform lookup после отмены — `calls == 0`.
- [ ] **RED:** `cd server && go test ./internal/service ./internal/bot -run 'Test(SubscriptionFetcher|Fetch|LazyTunnel|GetSubscription|RefusePrivatePeer|NewTunnelHTTPClient|ServersFromSubscription)' -count=1` — shared fetcher отсутствует.
- [ ] **Implement:** Перенести fetch/decode orchestration в `SubscriptionFetcher.Fetch`, сохранив HTTPS-only, 1 MiB body cap, WAN→tunnel fallback, IPv4-only lookup, lazy tunnel selection и исходные error types. Внутренний `fetchServers(ctx, rawURL, wan, tunnel, wanLookup, tunnelLookup)` сохраняет существующую сигнатуру в service для перенесённых tests. `netpath` остаётся свободным от service/bot imports.
- [ ] **GREEN:** Та же команда, затем `cd server && go test ./internal/service ./internal/bot ./internal/netpath -count=1` — PASS.
- [ ] **Commit:** Stage только Files задачи и удаления пустых wrappers. Subject: `refactor(service): share subscription fallback fetching`; body: `Reuse one guarded fetch and resolution path for subscription automation and its callers.`

### Task 3: Реализовать persistent notification store

**Files:** Create `server/internal/notifications/store.go`, `server/internal/notifications/persist.go`, `server/internal/notifications/recipients.go`, `server/internal/notifications/store_test.go`, `server/internal/notifications/persist_test.go`, `server/internal/notifications/recipients_test.go`, `server/internal/watchdapi/watch_types.go`.

**Interfaces:** Consumes resolved `ConfigService.DataDir()` и `time.Time`. Produces типы ниже с snake_case JSON names; `WatchState` values — `starting`, `active`, `stopped`, `incompatible`, `error`, клиентский `not_running`:
```go
// package watchdapi
type WatchState string
type EventID string // <32 lowercase hex store epoch>:<positive decimal sequence>
type Recipient struct { ChatID int64; FirstSeen time.Time }
type Notification struct { ChatID int64; EventID EventID; At time.Time; Text string }
type NotificationPage struct { Messages []Notification; NextCursor string }
type NotificationsStatus struct { Pending int; StorageError string }
type WatchSnapshot struct {
    State WatchState; UpdatedAt time.Time; Message, Action string
    CommittedFailover, PendingRestore bool
    Notifications NotificationsStatus
}
// package notifications
func NewStore(path string, now func() time.Time) (*Store, error)
func (s *Store) Publish(text string) (eventID watchdapi.EventID, err error)
func (s *Store) ReplaceRecipients(recipients []watchdapi.Recipient) error
func (s *Store) Pending(cursor string) (watchdapi.NotificationPage, error)
func (s *Store) Ack(chatID int64, eventID watchdapi.EventID) error
func (s *Store) Status() watchdapi.NotificationsStatus
func (s *Store) Flush() error
func (s *Store) Run(ctx context.Context)
```
- [ ] **Test:** `TestStore_RestartTTLAndCap`: publish 21 событий для чата даёт `len(Messages) == 20`, `Messages[0].EventID == publishedIDs[1]`; возраст ровно `12*time.Hour` сохраняет событие, возраст больше TTL удаляет. `TestRecipients_FirstSeenRevokeAndRejoin`: история ограничена 20/12h, новый чат получает только `At >= FirstSeen`, acked ID после revoke/rejoin отсутствует. `TestAck_DirtyIntentSurvivesLogicalRemoval`: ошибка rename даёт `err != nil`, `Status().StorageError != ""`; повтор ack возвращает ошибку до успешного `Flush`, после открытия файла событие отсутствует. `TestStore_CorruptOriginalIsPreserved` сохраняет исходные bytes; `TestStore_CorruptionDoesNotReuseIDs` даёт `newID != oldID` даже при одинаковом injected clock; `TestStore_AtomicHealthSchema`: version 1, mode `0600`, monotonic sequence, recent/recipient/closed-progress/health sections survive round-trip.
- [ ] **RED:** `cd server && go test ./internal/notifications -count=1` — пакет/store отсутствуют.
- [ ] **Implement:** Store сериализует изменения RAM mutex-ом, сохраняет version 1 с отдельным random store epoch, монотонной event sequence, recent events, получателями, per-chat pending/closed progress и subscription health. Epoch сохраняется при штатном restart; новая очередь после потери/повреждения файла получает новый epoch и сохраняет уникальность event IDs. Atomic IO выполняется с сериализацией saves вне state mutex; dirty revision очищается только для действительно сохранённой версии. Cap/TTL действуют в RAM при ошибке файла; закрытый прогресс ограничен сроком истории. Пустой список отзывает всех. Logical ack и removal составляют одну durable запись; повтор ack пытается сохранить dirty state. `NewStore` возвращает доступный bounded store вместе с безопасной диагностикой при повреждении/ошибке чтения; повреждённый файл сохраняется в отдельном уникальном backup до замены. `Run` повторяет dirty save раз в 10s и делает последнюю попытку при cancellation.
- [ ] **GREEN:** Та же команда и `cd server && go test -race ./internal/notifications -count=1` — PASS, включая injected write/sync/rename failures и сохранность предыдущего файла при неудачной публикации.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(notifications): persist watch events and delivery progress`; body: `Keep bounded per-chat queues and recent event history with atomic durable acknowledgments.`

### Task 4: Расширить локальный Unix API

**Files:** Create `server/internal/watchdapi/notifications_server.go`, `server/internal/watchdapi/notifications_client.go`, `server/internal/watchdapi/notifications_server_test.go`, `server/internal/watchdapi/notifications_client_test.go`; Modify `server/internal/watchdapi/server.go`, `server/internal/watchdapi/client.go`; Modify `server/internal/notifications/store.go`, `server/internal/notifications/store_test.go` для cursor validation/pagination.

**Interfaces:** Consumes Task 3. Produces отдельные контракты, сохраняя старый `API`:
```go
// package watchdapi
type WatchAPI interface { Watch(context.Context) (WatchSnapshot, error) }
type NotificationAPI interface {
    SetRecipients(context.Context, []Recipient) error
    Pending(context.Context, string) (NotificationPage, error)
    Ack(context.Context, int64, EventID) error
}
type AutomationSource interface {
    WatchSnapshot() WatchSnapshot
    ReplaceRecipients([]Recipient) error
    Pending(string) (NotificationPage, error)
    Ack(int64, EventID) error
}
func NewHandler(src Source, automation ...AutomationSource) http.Handler
func ServeListener(ctx context.Context, l net.Listener, src Source, automation ...AutomationSource) error
// *Client implements WatchAPI and NotificationAPI with the exact method signatures above.
```
- [ ] **Test:** `TestNotificationsAPI_ValidationIsAtomic`: null/missing/wrong-type/trailing JSON, zero ChatID, duplicate ChatID, zero/future `FirstSeen`, malformed/zero-sequence/future-sequence event ID → `status == 400`, предыдущие recipients/pending неизменны; negative Telegram ChatID допустим. Body `> 1<<20` → `413`. `TestPending_PaginationUnderMutation`: `len(Messages) <= 100`, encoded response `< 16<<20`, read сохраняет очередь, fresh empty-cursor pass доставляет вставленные между страницами события; invalid/over-256 cursor → 400. `TestNotificationClient_DeadlineAndBodyCap`: hung socket заканчивается за 2s, oversized/trailing response отвергается. Existing `Test*Monitor*` продолжают прежние assertions/statuses.
- [ ] **RED:** `cd server && go test ./internal/watchdapi ./internal/notifications -run 'Test(NotificationsAPI|Pending|NotificationClient)' -count=1` — routes/methods отсутствуют.
- [ ] **Implement:** Зарегистрировать четыре exact paths из spec §8. POST recipients принимает `{"recipients": [...]}`, explicit `[]` отзывает всех; Client нормализует nil recipients в explicit `[]`, `TestNotificationClient_EmptyRecipientsRevokesAll` проверяет точный JSON; ack — `chat_id/event_id`. Success mutations — `200 {"ok":true}`; storage failure — 503 с безопасным текстом. Синтаксис EventID определён в задаче 3. Unknown/expired/closed ack в пределах выданной sequence идемпотентен; future sequence текущего epoch — 400. ID предыдущего epoch считается закрытым и сохраняет новую очередь. Cursor кодирует последнюю пару `(chat_id,event_id)`; ChatID сортируется численно, события одного чата — по численной sequence, новая страница ограничивается числом и encoded size. Strict decoder завершает весь JSON до mutation. Новые методы Client имеют отдельный 2s context и проверяют полный bounded response; handlers работают независимо от slow bot send.
- [ ] **GREEN:** `cd server && go test -race ./internal/watchdapi ./internal/notifications -count=1` — PASS; старые `NewHandler(src)` и `ServeListener(ctx,l,src)` callers/fakes компилируются.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(watchdapi): expose watch notifications over the private socket`; body: `Add bounded recipient synchronization, paginated reads and idempotent acknowledgments.`

### Task 5: Подключить bot receiver и актуальных получателей

**Files:** Create `server/internal/bot/notifications.go`, `server/internal/bot/notification_text.go`, `server/internal/bot/notifications_test.go`; Modify `server/internal/chatstore/store.go`, `server/internal/chatstore/store_test.go`, `server/internal/bot/bot.go`, `server/internal/bot/bot_test.go`, `server/internal/bot/outbox.go`, `server/internal/bot/outbox_test.go`.

**Interfaces:** Consumes `watchdapi.NotificationAPI`, existing `telegram.MessageSender` и `Auth`. Produces:
```go
// package chatstore: add FirstSeen time.Time to UserChat, returned by GetActiveUsers.
func (s *Store) SetInactiveChat(chatID int64) error
// package bot
func activeRecipients(store *chatstore.Store, auth *Auth) []watchdapi.Recipient
func withNotifications(api watchdapi.NotificationAPI) Option
func (b *Bot) syncRecipients(ctx context.Context) error
func (b *Bot) pollNotifications(ctx context.Context) error
func (b *Bot) receiveNotifications(ctx context.Context)
func notificationText(n watchdapi.Notification, now time.Time) string
```
- [ ] **Test:** `TestRecipients_AuthorizedEarliestFirstSeen`: duplicated chat records дают один ChatID с минимальным временем только разрешённых активных записей. `TestReceiver_SyncBeforeGetMe`: fake IPC получает recipients перед первой Telegram попыткой, включая её failure. `TestReceiver_AckRetryAndIndependentChats`: после успешного send и неудачного ack `sendCount == 1`, `ackAttempts == 2`; blocked/slow chat сохраняет доставку второму; 429/5xx retry сохраняет порядок. `TestReceiver_RevocationAndPaging`: перед send перечитывается authorization, inactive recipient получает `sendCount == 0`, новый проход начинается `cursor == ""`. `TestNotificationText_MinuteBoundary`: 59s сохраняет текст, 60s добавляет `(15:04, delayed)`, другой день — прежний `Jan 2 15:04` формат; каждый Telegram block соблюдает `telegram.MaxMessageLength`.
- [ ] **RED:** `cd server && go test ./internal/bot ./internal/chatstore -run 'Test(Recipients|Receiver|NotificationText)' -count=1` — receiver/FirstSeen access отсутствуют.
- [ ] **Implement:** Sync и receiver запускаются один раз из `Bot.New`, до `Connect`/ожидания Telegram; activity changes вызывают sync, poll повторяет его каждые 10s. Sender/pathLive читаются безопасно. В одном чате отправки последовательны; разные чаты обслуживаются независимыми jobs с bounded concurrency. Уже отправленные события ждут повторного ack в RAM даже если следующий pending read пустой; TTL/revoke ограничивают эту память. Использовать bounded plain sender, подтвердить событие после всех successful blocks; сохранить permanent-error правила и деактивировать все записи заблокированного ChatID. Existing bot watch/outbox остаются до атомарного переключения ownership в задаче 8.
- [ ] **GREEN:** `cd server && go test -race ./internal/bot ./internal/chatstore ./internal/telegram -count=1` — PASS; сохранены existing outbox tests до migration и startup/update notifications.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(bot): receive durable watch notifications`; body: `Synchronize authorized chats and retry delivery acknowledgments without blocking unrelated recipients.`

### Task 6: Закрепить capabilities и mutation gate

**Files:** Create `server/internal/watchcompat/capabilities.go`, `server/internal/watchcompat/gate.go`, `server/internal/watchcompat/proc_linux.go`, `server/internal/watchcompat/proc_other.go`, `server/internal/watchcompat/gate_test.go`, `server/internal/watchcompat/capabilities_test.go`, `server/internal/subwatch/mutation.go`, `server/internal/subwatch/mutation_test.go`; Modify `server/internal/subwatch/watch.go`, `server/internal/service/activeserver.go`, `server/internal/service/activeserver_test.go`, `server/internal/service/xray.go`, `server/internal/service/xray_test.go`.

**Interfaces:** Consumes `/proc`, existing config generation/locks, Task 1 readiness. Produces:
```go
// package watchcompat
const ProtocolVersion = 1
type Capabilities struct { ProtocolVersion int; WatchOwner string } // protocol_version, watch_owner
func WriteCapabilities(w io.Writer) error // JSON {"protocol_version":1,"watch_owner":"watchd"}
var ErrIncompatible = errors.New("bot compatibility is unconfirmed")
type Gate struct { BotPath, ProcRoot string } // private cache and injected proc/exec seams live here
func (g *Gate) Check(ctx context.Context) error // nil means automation may mutate
// package subwatch: add Watch.CanMutate func() error; nil preserves existing test defaults.
func (w *Watch) mutationAllowed() error
// package service
type GuardedXrayGenerator interface {
    GenerateConfigGuarded(vpnconfig.Server, InboundPorts, func() error) error
}
func (s *XrayService) GenerateConfigGuarded(server vpnconfig.Server, ports InboundPorts, beforeCommit func() error) error
func GenerateAndRecordGuardedWalkedServer(store ConfigStore, xray GuardedXrayGenerator, generate, identity vpnconfig.Server, ports InboundPorts, guard func(*vpnconfig.VPNDirectorConfig) error) (generated bool, seq int, err error)
```
- [ ] **Test:** `TestCapabilities_JSONContract`: exact JSON `{"protocol_version":1,"watch_owner":"watchd"}`, valid EOF, безопасная ошибка writer. Активация CLI flag выполняется в задаче 8 вместе с удалением bot watch; текущий бот до этого сохраняет фактическое ownership. `TestGate_InstalledAndRunningExecutables`: missing bot+no process → nil; compatible stopped → nil; old/invalid/timed-out/unreadable → incompatible; replaced path+old `/proc/PID/exe` → incompatible; PID reuse/vanishing process rechecked; mention bot path только в later argv не считается bot process. `TestMutation_GateAfterWaitAndValidation`: gate меняется после lock wait или внутри injected validation; `generated == false`, live config/selection unchanged, `restartCalls == 0`.
- [ ] **RED:** `cd server && go test ./internal/watchcompat ./internal/subwatch ./internal/service -run 'Test(Capabilities|Gate|Mutation|GenerateConfigGuarded|GenerateAndRecordGuarded)' -count=1` — capability/gate/guarded API отсутствуют.
- [ ] **Implement:** Реализовать pure capability writer и compatibility gate. Gate проверяет installed executable и каждый реально работающий bot executable через `/proc/PID/exe`; capability subprocess запускается из `/`, с 2s deadline и stdout cap 4096 bytes, strict JSON и exact protocol/owner. Cache ключует device/inode/size/mtime и PID start identity; изменение состава/неопределённость заставляют перепроверить и закрывают gate. `CanMutate` проверяется на tick, после waits, в locked updates/generation, перед/после apply/restart; stop-poll отменяет также операцию, потерявшую совместимость. `endsWalk`/switcher прекращают попытку при `watchcompat.ErrIncompatible` и cancellation; следующая совместимая Tick начинает новое подтверждение отказа. Guarded generator повторяет guard после Xray validation непосредственно перед live rename под тем же config lock; существующий `GenerateConfig` API сохраняется.
- [ ] **GREEN:** `cd server && go test -race ./internal/watchcompat ./internal/subwatch ./internal/service -count=1` — PASS; existing manual generation semantics, temp cleanup и returned sequence assertions сохранены.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(watchd): gate automation on bot capabilities`; body: `Verify installed and running bot executables and recheck mutation guards before publishing Xray configuration.`

### Task 7: Сохранить незавершённое восстановление клиентов

**Files:** Create `server/internal/vpnconfig/pending_restore.go`, `server/internal/vpnconfig/pending_restore_test.go`, `server/internal/subwatch/recovery.go`, `server/internal/subwatch/recovery_test.go`; Modify `server/internal/vpnconfig/vpnconfig.go`, `server/internal/vpnconfig/failover.go`, `server/internal/subwatch/watch.go`, `server/internal/subwatch/watch_test.go`.

**Interfaces:** Consumes Task 6 mutation guards, existing `XrayFailover`, `ActiveServer`, config lock и readiness. Produces:
```go
// package vpnconfig: XrayConfig.PendingRestore *XrayPendingRestore, json pending_restore,omitempty.
type XrayPendingRestore struct { Snapshot *XrayFailover; Restored []string; Active *ActiveServer }
// package subwatch
func (w *Watch) reconcileRestore(cfg *vpnconfig.VPNDirectorConfig) error
```
- [ ] **Test:** `TestRecovery_PendingRestoreAfterRestart`: stop после config restore и failed Apply, новый watch с missing TPROXY сохраняет fallback snapshot и retry. `TestRecovery_UnarmedAndManualChanges`: отсутствие подписок допускает завершение intent; changed active seq, paused/deleted/moved client исключают устаревший rollback. `TestPendingRestore_RoundTripAndDetach`: JSON round-trip сохраняет intent, `DetachFailoverClient` удаляет адрес из snapshot/restored, остальные assignments сохраняются; повторный successful recovery оставляет `PendingRestore == nil` и один restore event.
- [ ] **RED:** `cd server && go test ./internal/vpnconfig ./internal/subwatch -run 'Test(Recovery|PendingRestore)' -count=1` — restore metadata отсутствует, новый watch теряет RAM-only intent.
- [ ] **Implement:** Записывать минимальный `xray.pending_restore` в той же locked mutation, которая удаляет failover; identity/seq guard действует перед записью и final clear. Pending intent arms завершение существующего restore при отсутствии подписок/клиентов. Startup сверяет текущие assignments, stop, seq и readiness; clear выполняется после successful Apply/readiness либо guarded reinstatement через существующий `ApplyFailoverSnapshot`. `DetachFailoverClient` фильтрует pending intent; paused/deleted/moved адреса сохраняют ручное назначение. Metadata содержит только routing snapshot и active identity; credentials отсутствуют. Текущий bot-owned watch уже получает этот recovery механизм; ownership меняется в следующей задаче.
- [ ] **GREEN:** `cd server && go test -race ./internal/vpnconfig ./internal/subwatch -count=1` — PASS, existing restore/readiness/make-before-break assertions сохранены.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(subwatch): persist guarded pending restore intent`; body: `Recover interrupted client restores from configuration while respecting readiness and newer manual assignments.`

### Task 8: Перенести watch lifecycle в watchd

**Files:** Create `server/cmd/watchd/runtime.go`, `server/cmd/watchd/watch.go`, `server/cmd/watchd/runtime_test.go`, `server/internal/subwatch/status.go`, `server/cmd/bot/main_test.go`, `server/internal/service/context.go`, `server/internal/service/context_test.go`; Modify `server/cmd/watchd/main.go`, `server/cmd/watchd/main_test.go`, `server/internal/paths/paths.go`, `server/internal/paths/paths_test.go`, `server/internal/subwatch/watch.go`, `server/cmd/bot/main.go`, `server/internal/service/xray.go`, `server/internal/service/xray_test.go`, `server/internal/bot/bot.go`, `server/internal/bot/bot_test.go`, `server/internal/bot/outbox.go`, `server/internal/bot/outbox_test.go`.

**Interfaces:** Consumes Tasks 1–7, `subwatch.Watch.Start`, `monitor.Run`, `ConfigService`, `VPNDirectorService`, `XrayService`. Produces:
```go
// package paths: add BotBinary, TunnelTables, FailoverReady, TPROXYReady string;
// production paths match current bot defaults, dev paths stay inside testdata/dev.
// package subwatch
func (w *Watch) Snapshot() watchdapi.WatchSnapshot
// package service: existing interfaces stay unchanged; root cancellation reaches shell/Xray validation.
func WithContext(root context.Context, executor ShellExecutor) ShellExecutor
func NewXrayServiceForContext(ctx context.Context, templatePath, outputPath string) *XrayService
// package main (cmd/watchd)
type runtimeDeps struct { Monitor daemonMonitor; Watch *subwatch.Watch; Queue *notifications.Store }
func runRuntime(ctx context.Context, socket string, build func() (runtimeDeps, error)) error
func newWatch(ctx context.Context, p paths.Paths, cfg *service.ConfigService, vpn *service.VPNDirectorService, xray *service.XrayService, q *notifications.Store, gate *watchcompat.Gate) *subwatch.Watch
```
- [ ] **Test:** `TestRuntime_OwnershipBeforeEverySideEffect`: duplicate instance даёт `buildCalls == 0`, queue writes/prober cleanup/watch mutation — 0; owner shutdown ждёт monitor/watch/queue до освобождения lock. `TestRuntime_BotIndependentAndStopped`: bot absence/cancellation сохраняет watch ticks; stopped marker даёт `mutationCalls == 0`, доставка existing events доступна; `monitor.enabled=false` даёт legacy failover. `TestRuntime_StatusDuringBlockedProbe`: `Watch`/`Monitor` отвечают до 2s во время долгого Tick. `TestRuntime_StorageFailureKeepsAutomation`: unreadable/corrupt/failed-save queue возвращает bounded store с диагностикой, `watchTicks > 0` и monitor API доступен. `TestCapabilities_EarlyReadOnlyCLI`: flag завершается до token/platform/network services с `calls == 0`, `self-update` first argument сохраняет приоритет. `TestContextExecutor_ShutdownCancelsOperations`: root cancellation отменяет bounded shell/platform/Xray validation, новая config публикация отсутствует. Existing watch/return/wave assertions остаются; `TestNew_WatchStartsWhenGetMeFails` заменяется утверждением `botWatchStarts == 0` и живым receiver.
- [ ] **RED:** `cd server && go test ./cmd/watchd ./cmd/bot ./internal/service ./internal/subwatch ./internal/vpnconfig ./internal/bot -run 'Test(Runtime|Capabilities_Early|ContextExecutor|New_)' -count=1` — runtime/scoped services/новое ownership отсутствуют.
- [ ] **Implement:** Приобрести `watchdapi.Listen` до вызова build, queue load/save, prober cleanup и watch writes. Запустить monitor/watch/queue с общим context, API adapter объединяет monitor Source, watch Snapshot и queue methods; API failure отменяет все компоненты, listener lock живёт до их shutdown. Store initialization error с доступным RAM store диагностируется и сохраняет запуск runtime; ошибка разрешения data path также даёт RAM-only очередь с явным storage_error. В production использовать настоящий executor и platform export; `--dev` — `devmode.Executor`, fake prober, dev readiness paths и compatible synthetic gate. Добавить watchd `--platform` по существующему daemon pattern, сохранив ранний `self-update`. Snapshot хранится под отдельным коротким status mutex и читается во время долгого Tick. В том же commit активировать bot `--watchd-capabilities` после early self-update и до platform/config/network; flag описывает фактическое удаление bot watch. Shell/platform executor объединяет root context с command timeout; Xray validation получает root context, сохраняя 15s timeout. `newWatch` использует scoped services и повторяет текущую bot DI с shared fetch/readiness, guarded generation и Notify→Publish. Bot прекращает создание/start subwatch; старый outbox заменяется receiver, существующие delayed/retry assertions переносятся.
- [ ] **GREEN:** `cd server && go test -race ./cmd/watchd ./internal/subwatch ./internal/vpnconfig ./internal/bot ./internal/paths ./internal/service ./cmd/bot -count=1` — PASS; signal/socket failure сохраняют current main Xray/routing, queue flush failure диагностируется и shutdown заканчивается.
- [ ] **Commit:** Stage только Files задачи и удаления outbox файлов после переноса оставшихся assertions. Subject: `feat(watchd): own subscription failover independently of Telegram`; body: `Run monitoring, guarded subscription automation and notification storage under one cancellable daemon lifetime.`

### Task 9: Дать monitor проверяемое свидетельство свежего результата

**Files:** Create `server/internal/monitor/evidence.go`, `server/internal/monitor/evidence_test.go`; Modify `server/internal/monitor/monitor.go`, `server/internal/monitor/entry.go`, `server/internal/monitor/harness_test.go`.

**Interfaces:** Consumes current `Monitor.Check`, session lifecycle, completion sequence и watchdapi statuses. Produces:
```go
// package monitor: token is private and ties the value to its originating Monitor.
type Evidence struct { State watchdapi.State; Endpoints map[string]watchdapi.EndpointState; token evidenceToken }
type evidenceToken struct { owner *Monitor; session Session; generation uint64; records map[string]evidenceRecord }
type evidenceRecord struct { completion, revision uint64; status watchdapi.Status }
func (m *Monitor) Evidence() Evidence
func (m *Monitor) CheckEvidence(ctx context.Context, keys []string) (Evidence, error)
func (m *Monitor) ValidateEvidence(e Evidence, keys []string) error
```
- [ ] **Test:** `TestEvidence_OnlyAfterRequestInLiveGeneration`: in-flight check, начатый до call, требует follow-up; restored/old-session результаты не fresh. `TestEvidence_InvalidatesOnCrashStateAndKeyChange`: stop/disable/WAN/prober error, changed required status/key/session дают `err != nil`; order-only/active-only rebuild сохраняет generation. `TestEvidence_MissingRejectedAndTimeout`: missing/unknown active key остаётся incomplete, rejected отличается от fresh dead, context deadline завершает ожидание. `TestEvidence_AllRejectedCurrentBuild`: static rejected сохраняет current applicability, после изменения outbound теряет её. `TestEvidence_DoesNotBlockIPC`: revalidation не держит monitor mutex во время config generation/validation.
- [ ] **RED:** `cd server && go test ./internal/monitor -run TestEvidence -count=1` — Evidence API отсутствует.
- [ ] **Implement:** Evidence содержит immutable копию records и private proof: monitor identity, current endpoint-set/session generation и completion/revision каждого ключа. `Evidence()` включает только записи, применимые к текущему набору/поколению; persisted rejected переоцениваются по текущему build. Generator refusal действует в текущем endpoint-set generation даже при отсутствии checkable endpoints/session; alive/dead evidence требует живого session. `CheckEvidence` использует существующую fresh queue и worker pool, фиксирует один live generation; generation/state change делает вызов inconclusive. `ValidateEvidence` сравнивает requested records и соответствующий generation/session под коротким mutex и возвращает безопасную stale/inactive ошибку. Исходный `Check(ctx,keys)` сохраняет прежнюю сигнатуру и assertions.
- [ ] **GREEN:** `cd server && go test -race ./internal/monitor ./internal/watchdapi -count=1` — PASS, включая late completion, crash, WAN guard и existing scheduling tests.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(monitor): expose revalidatable fresh check evidence`; body: `Bind endpoint results to a live prober generation and reject stale evidence before automatic mutations.`

### Task 10: Добавить раннее прямое переключение

**Files:** Create `server/internal/subwatch/fast.go`, `server/internal/subwatch/fast_test.go`; Modify `server/internal/subwatch/watch.go`, `server/internal/subwatch/watch_test.go`, `server/cmd/watchd/watch.go`.

**Interfaces:** Consumes Tasks 6–9, `walkOrder`, `endpoint.PerAddress/Key/Keys/ServerForDial`, `monitor.WANUp`. Produces:
```go
// package subwatch
const FastCandidates = 3
const FastCheckTimeout = 30 * time.Second
type HealthMonitor interface {
    Evidence() monitor.Evidence
    CheckEvidence(context.Context, []string) (monitor.Evidence, error)
    ValidateEvidence(monitor.Evidence, []string) error
}
// Add Watch.Health HealthMonitor and Watch.WANUp func(context.Context) bool; nil uses legacy.
type fastOutcome uint8 // fastInconclusive, fastSwitched, fastFallback, fastCanceled
type fastAttempt struct { Config *vpnconfig.VPNDirectorConfig; Outcome fastOutcome; Guard func(*vpnconfig.VPNDirectorConfig) error }
func (w *Watch) fastFailover(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) fastAttempt
func (w *Watch) failOutbound(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, reason string, guard func(*vpnconfig.VPNDirectorConfig) error)
```
- [ ] **Test:** `TestFast_FirstFailureSwitchesWithoutClientMove`: all active keys fresh dead, WAN alive, candidate alive → `restartCalls == 1`, `applyCalls == 0`, Xray clients/TPROXY unchanged, original choice remains preferred, one server-change event. `TestFast_OrderDedupAndBudget`: cached alive candidates follow `walkOrder`/`PerAddress`, exclude active keys, dedup by key, `candidateChecks <= 3`, all CheckEvidence/WAN calls share `deadline-start <= 30*time.Second`. `TestFast_InconclusiveUsesLegacy`: partial active death, missing/empty/rejected, WAN down, crash or timeout produce `mutationCalls == 0` before existing 1m/3m timer; healthy main HTTPS probe делает `freshChecks == 0`. `TestFast_GuardsAndFailureFallback`: stop, same-server seq increment, link deletion/rotation, changed outbound under same name, or evidence invalidation at final validation → old selection untouched; Generate/restart/main probe failures exercise next candidate and guarded tunnel fallback with readiness; restore message отсутствует при неизменном client assignment. `TestFast_GeneratedButRecordSaveFailed` сохраняет существующую семантику `generated == true`: main restart выполняется, возвращённый прежний seq сохраняется и чужой Select не усваивается.
- [ ] **RED:** `cd server && go test ./internal/subwatch -run TestFast -count=1` — first failed primary probe сохраняет legacy ожидание/новых методов нет.
- [ ] **Implement:** После первого failed main probe вызвать fast path один раз за обычный tick. Фиксировать exact active identity/seq, subscription links и актуальные endpoint keys; найти совпадающую запись целиком. Запрос active set, individual candidates и bounded WAN controls выполняются параллельно в одном 30s context. Active death требует полного fresh dead set в одном live generation; direct candidate требует complete fresh alive evidence того же generation. Если usable candidate отсутствует и любой нужный результат incomplete, использовать legacy; полный all-dead результат без кандидата даёт immediate fallback. Перед каждым Generate и final live rename проверить mutation gate, current record/seq/link, exact current candidate dial key и обе evidence; обновлять expected seq только собственным успешным record.
- [ ] **Implement transition:** Direct switch использует существующие guarded generation, main-process restart, 3s settle и HTTPS SOCKS probe. Try до трёх fresh candidates в сохранённом порядке; если main проверки не прошли, перейти к `failOutbound` с сохранённым ownership guard. Этот метод выделяет существующий fallback блок Tick, сохраняя guards на stage/commit/refresh/walk и make-before-break. Superseded/stop/compatibility errors прекращают попытку; infrastructure/stale evidence возвращают legacy clock без дополнительного цикла.
- [ ] **GREEN:** `cd server && go test -race ./internal/subwatch ./cmd/watchd ./internal/service -count=1` — PASS; assertions legacy timers, subscription guards и preferred return сохранены.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(subwatch): switch early using fresh monitor evidence`; body: `Confirm active endpoint failure and try bounded healthy alternatives while preserving routing and manual-selection guards.`

### Task 11: Приоритизировать здоровье в обычном walk

**Files:** Create `server/internal/subwatch/health_order.go`, `server/internal/subwatch/health_order_test.go`; Modify `server/internal/subwatch/watch.go`, `server/internal/subwatch/wave_test.go`, `server/internal/subwatch/return_test.go`.

**Interfaces:** Consumes Task 10 `HealthMonitor`, `walkOrder`, `endpoint.PerAddress` и existing switcher/return contracts. Produces `func healthOrder(servers []vpnconfig.Server, evidence monitor.Evidence) []vpnconfig.Server`.

- [ ] **Test:** `TestHealthOrder_StableGroupsAndCurrentRejections`: последовательность `[unknown1, alive1, dead1, alive2, rejected1]` даёт `[alive1, alive2, unknown1, dead1]`; stable order сохраняется после `OwnFirst`/round-robin/PerAddress; missing keys остаются unknown, latency порядок не меняет. `TestWalk_HealthyFirstStillProbesMain`: cached alive с failed main probe продолжает walk; infrastructure/unavailable health восстанавливает прежний порядок, stale rejected вновь допускается. `TestReturn_HealthOrderingPreservesBackoffAndChoice`: existing 5m/10–30m/4 failures и manual selection assertions сохранены.
- [ ] **RED:** `cd server && go test ./internal/subwatch -run 'Test(HealthOrder|Walk_HealthyFirst|Return_HealthOrdering)' -count=1` — ordering helper отсутствует/обычный walk следует исходному порядку.
- [ ] **Implement:** После `PerAddress(walkOrder(...))` stable-partition current live evidence: alive первым, unknown/dead вторым, current rejected исключаются. Перед исключением rejected подтвердить evidence применимость; при stale/global non-ok состоянии использовать исходный порядок. `healthOrder` вызывается только с Evidence, прошедшей `ValidateEvidence`; перед каждой генерацией актуализировать ordering оставшихся непроверенных keys, чтобы поздняя rejection/generation change сохраняла правильную применимость. Каждый candidate сохраняет main HTTPS probe и существующую dedup/Generate guard семантику. Preferred выбран из исходного walk order; fallback возврат к нему и scheduled return сохраняют свои прежние правила.
- [ ] **GREEN:** `cd server && go test -race ./internal/subwatch -count=1` — PASS, включая полный order/wave/return suite.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(subwatch): prioritize monitored healthy walk endpoints`; body: `Preserve stable walk order within health groups and retain main-Xray verification and preferred-return behavior.`

### Task 12: Публиковать переходы здоровья подписок

**Files:** Create `server/internal/notifications/subscription_health.go`, `server/internal/notifications/subscription_health_test.go`, `server/cmd/watchd/health.go`, `server/cmd/watchd/health_test.go`; Modify `server/cmd/watchd/runtime.go`, `server/cmd/watchd/runtime_test.go`, `server/internal/notifications/persist.go`.

**Interfaces:** Consumes Task 3 store, `watchdapi.Health`, `endpoint.Keys`, current subscriptions и monitor Snapshot. Produces:
```go
// package notifications
func (s *Store) ObserveSubscriptions(subs []vpnconfig.Subscription, snapshot watchdapi.Snapshot) error
// package main (cmd/watchd)
func publishSubscriptionHealth(ctx context.Context, load func() ([]vpnconfig.Subscription, error), source watchdapi.Source, queue *notifications.Store)
```
- [ ] **Test:** `TestSubscriptionHealth_CompleteTransitionsOnce`: первая полная all-dead/rejected scan → 1 event; repeat/rename/restart → 0 дополнительных; later any alive → 1 recovery; duplicate endpoint aliases и multiple addresses fold via Health. `TestSubscriptionHealth_UnknownAndInfrastructureFreeze`: каждый non-ok monitor state, empty subscription, unknown/missing/new refresh key сохраняют previous health и `newEvents == 0`. `TestSubscriptionHealth_AtomicFailureAndDelete`: injected save failure удерживает RAM health+event+sequence как один intent, retry/restart после successful flush даёт один переход; deleted ID теряет health record, его историческое сообщение живёт до ack/TTL.
- [ ] **RED:** `cd server && go test ./internal/notifications ./cmd/watchd -run 'TestSubscriptionHealth|TestPublishSubscriptionHealth' -count=1` — observer/publisher отсутствуют.
- [ ] **Implement:** Fold по актуальным IDs и server keys; any alive подтверждает ненулевое состояние, ноль требует непустого полного набора dead/rejected. Состояние subscription, event sequence и queue append меняются под одним store mutex и сохраняются одной atomic записью. Safe event copy: `Subscription <name> has no live servers` / `Subscription <name> has a live server again`. `TestPublishSubscriptionHealth_LoadFailureKeepsPrevious` требует `newEvents == 0` после transient read error и recovery следующего успешного load. Publisher начинает сразу и затем каждые 10s, прекращается с runtime; ошибки загрузки сохраняют предыдущую отметку. Store error остаётся диагностикой и сохраняет работу watch.
- [ ] **GREEN:** `cd server && go test -race ./internal/notifications ./cmd/watchd ./internal/watchdapi -count=1` — PASS, включая WAN/restart/file-failure paths.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(watchd): notify subscription health transitions`; body: `Persist confirmed zero-live and recovery events atomically with subscription health and delivery queues.`

### Task 13: Показать watch отдельно в HTTP API и /status

**Files:** Create `server/internal/webapi/handler_watch.go`, `server/internal/webapi/handler_watch_test.go`, `server/internal/handler/watch.go`, `server/internal/handler/watch_test.go`; Modify `server/internal/webapi/router.go`, `server/cmd/webui/main.go`, `server/internal/handler/handler.go`, `server/internal/handler/status.go`, `server/internal/handler/status_test.go`, `server/internal/bot/bot.go`, `server/internal/subwatch/status.go`; Create `server/internal/subwatch/status_test.go`.

**Interfaces:** Consumes `watchdapi.WatchAPI`, Monitor `API`, Task 8 Snapshot/queue adapter. Produces `webapi.Deps.Watch watchdapi.WatchAPI`, `handler.Deps.Watch watchdapi.WatchAPI`, `func handleWatch(deps *Deps) http.HandlerFunc`, protected `GET /api/watch` → Task 3 `WatchSnapshot`; `func automationStatus(m watchdapi.API, w watchdapi.WatchAPI) string` in package handler.

- [ ] **Test:** `TestWatchSnapshot_AvailabilityAndAction`: states `starting/active/stopped/incompatible/error`, independent monitor disabled, committed failover/pending restore, safe action/message/error. `TestHandleWatch_AuthUnavailableAndStorageError`: authenticated read → 200; missing/old/down socket → `state == "not_running"`; auth middleware remains required; synthetic credential sentinel отсутствует в JSON. `TestStatus_MonitorAndWatchAreIndependent`: disabled monitor + active watch и incompatible/watch unavailable дают отдельные строки; existing status/manual restart/stop работают при IPC failure; общий deadline обоих IPC reads — 2s, reads выполняются параллельно.
- [ ] **RED:** `cd server && go test ./internal/webapi ./internal/handler ./internal/subwatch -run 'Test(WatchSnapshot|HandleWatch|Status_Monitor)' -count=1` — route/fields/status presentation отсутствуют.
- [ ] **Implement:** Watch availability определяется mutation gate и runtime, даже если watch unarmed; action values: `""`, `checking`, `switching`, `fallback`, `refreshing`, `walking`, `returning`, `restoring`. Runtime adapter добавляет queue Pending/StorageError. `GET /api/watch` предоставляет безопасный snapshot без mutation routes; socket errors дают клиентское not_running. `/status` сохраняет существующий shell code block и добавляет отдельный bounded plain monitor/watch section через parallel reads с одним 2s context; failed shell status тоже позволяет показать доступный automation section. `/logs watchd` и all logs сохраняют существующие sources и включают новую автоматику из watchd logger.
- [ ] **GREEN:** `cd server && go test -race ./internal/webapi ./internal/handler ./internal/subwatch ./internal/bot -count=1` — PASS, включая existing logs/manual command tests.
- [ ] **Commit:** Stage только Files задачи. Subject: `feat(status): expose independent watch and monitor state`; body: `Report automation availability, recovery intent and notification storage health through authenticated status surfaces.`

### Task 14: Отобразить состояния и проверить общие страницы в браузере

**Files:** Create `web/src/components/WatchStatus.vue`, `web/src/watch.ts`, `web/test/watch-status.cjs`; Modify `web/src/types.ts`, `web/src/api.ts`, `web/src/components/StatusTab.vue`, `web/src/components/ServersTab.vue`, `web/test/servers-monitor.cjs`, `web/package.json`.

**Interfaces:** Consumes Task 13 `GET /api/watch`. Produces TypeScript `WatchState` union из Task 3, `WatchResponse` с `state`, `updated_at`, `message?`, `action?`, `committed_failover`, `pending_restore`, `notifications: { pending: number; storage_error?: string }`; `api.getWatch()` — `GET /api/watch` с `timeout:8000`; `watchText(w: WatchResponse | null, unavailable: boolean): string`; `WatchStatus` props `{ snapshot: WatchResponse | null; unavailable: boolean }`.

- [ ] **Test:** По существующей dependency-free VM harness добавить `watch-status.cjs`: `disabled monitor + active watch` показывает обе строки; old success после new failure сохраняет `unavailable === true`; old failure после recovery сохраняет `state === "active"`; unmount исключает late mutation и polling. Test names: `watch state is independent`, `stale response cannot replace current state`, `unmount cancels polling`, `shared pages agree after controls`, `watch request has an 8000ms timeout`. Same watch response одинаково отображается на Status/Servers, storage error не скрывает active server/health; stop/apply/select и refresh обновляют соответствующие данные; existing checkNotice/fingerprint/409 tests сохраняют assertions. `api.getWatch` имеет `timeout === 8000`.
- [ ] **RED:** `cd web && node test/watch-status.cjs` — watch API/type/component отсутствуют.
- [ ] **Implement:** Status загружает status/IP/servers/monitor/watch независимо. Servers добавляет watch read к существующему 30s poll с отдельным request generation и lifecycle guards; monitor checkNotice logic сохраняется. Общий компонент использует `watchText`, показывает pending/storage error/action и безопасные причины ожидания. Состояния после controls перечитываются; отказ watch IPC сохраняет ручное управление. Расширить npm test двумя последовательными scripts, сохранив зависимости/lockfile.
- [ ] **GREEN:** `cd web && npm test && npm run build` — PASS (build включает `vue-tsc -b`); `make web-embed` через build runner. Все исходные UI regressions сохранены.
- [ ] **Browser:** Контроллер запускает synthetic dev app из отдельной `/tmp` копии committed checkout с fake executor/prober и её собственными dev paths, логинится и проверяет desktop 1440×900 и mobile 390×844: Status ↔ Servers ↔ Logs ↔ Clients ↔ Settings; stop/apply/restart, manual same-server selection, subscription add/refresh/rename/delete, one/all monitor checks, disabled monitor + active watch, mixed-version waiting, socket outage/recovery, storage error, pending restore, empty data и delayed responses. Проверить shared active badges/client assignments, watchd/all logs, отсутствие лишних mutation requests и timers после navigation; исправить и повторить обнаруженные regressions. При отсутствии browser tools явно зафиксировать покрытый substitute и оставшиеся непроверенными interactions.
- [ ] **Commit:** Stage только Files задачи после browser verification. Subject: `feat(web): show independent automation and notification status`; body: `Display watch availability consistently on status and server pages while preserving manual controls and monitoring flows.`

### Task 15: Закрепить установку, обновление и эксплуатационные правила

**Files:** Modify `install.sh`, `router/opt/etc/init.d/S98vpn-director-watchd`, `router/test/unit/install.bats`, `router/test/integration/vpn_director.bats`, `server/internal/updater/script_test.go`, `CLAUDE.md`, `.claude/rules/watchd.md`, `.claude/rules/telegram-bot.md`, `.claude/rules/webui.md`; Create `server/internal/updater/watch_migration_test.go`. Изменять `server/internal/updater/update_script.sh.tmpl` и `server/internal/updater/testdata/update_script.golden.sh` только при обнаруженной regression, относящейся к новым ownership contracts.

**Interfaces:** Consumes Task 6 capabilities, Task 8 runtime, existing daemon table, init `start/stop/restart/check`, updater running/stopped recovery и неизменный early `self-update` contract.

- [ ] **Test:** `TestWatchMigration_FullAndPartialUpdate`: first introduction watchd + compatible new bot → one automation owner; old installed/running bot или old deleted executable → new watch incompatible, monitoring/API доступны; partial copy/recovery/start failure сохраняют исходный running/stopped intent. `TestWatchMigration_MissingStoppedAndNoTokenBot`: absent bot и compatible stopped/no-token bot допускают watchd. Bats `watchd install: independent startup and visible partial installation`: optional failure выводит явную подсказку о недоступной автоматике; init shutdown сохраняет main Xray/markers; second instance сохраняет первый. Existing optional mv/chmod failure и bounded cleanup assertions сохраняются.
- [ ] **RED:** `cd server && go test ./internal/updater -run TestWatchMigration -count=1`; `bats router/test/unit/install.bats router/test/integration/vpn_director.bats` — новые transition assertions выявляют прежние startup/diagnostic ограничения. Existing updater intent tests служат regression controls.
- [ ] **Implement:** Уточнить installer/init сообщения: watchd обеспечивает и monitoring, и automatic failover; partial/missing daemon требует восстановления. Сохранить existing daemon table/order, webui-last table entry, bot-last successful startup и root-only paths. Документы описывают new ownership, capability waiting, независимый legacy failover при disabled monitor, durable queue/ack/storage failure, pending restore и раздельный статус. `CLAUDE.md` содержит стабильные команды/карту, `.claude/rules` — подробности. Шаблон updater менять только для подтверждённой migration failure, golden обновлять вместе с её regression test.
- [ ] **GREEN:** `cd server && go test ./internal/updater ./cmd/bot ./cmd/watchd ./internal/watchcompat -count=1`; та же targeted Bats команда одним процессом — PASS.
- [ ] **Commit:** Stage точные реально изменённые Files задачи. Subject: `feat(watchd): document and verify independent automation upgrades`; body: `Cover mixed-version installation and recovery and document the new daemon ownership and notification contracts.`

### Task 16: Общая приёмка и подготовка существующего PR

**Files:** Test весь scope предыдущих задач; planning cleanup касается только `docs/superpowers/specs/2026-10-05-server-monitor-stages-2-3-design.md` и `docs/superpowers/plans/2026-10-05-server-monitor-stages-2-3.md`. Evidence/logs/helpers — `/tmp` с проверенным владением; пользовательские файлы сохраняются.

**Interfaces:** Consumes результаты задач 1–15; produces проверенный branch diff для PR #63 и отчёт о выполненных/заблокированных аппаратных условиях.

- [ ] **Regression acceptance:** Дополнить owning tests любой найденной failure reproduction, пройти RED → fix → GREEN. Сверить spec §10 с test names: primary/prober/WAN failures; multi-address/duplicate keys; generation changes; stop/manual seq/refresh/delete; Generate/restart/apply/readiness failures; queue cap/TTL/restart/ack/storage/corrupt; recipient history/revoke; mixed-version/first-update/failed recovery; UI errors и recovery. Проверить fake executor isolation и сохранность owner files.
- [ ] **Full local verification:** Предварительно сохранить content/modes уже существующих generated artifacts либо выполнить build в собственной `/tmp` копии committed checkout; сохранить owner dev data. Команды ниже начинают работу от repository root. Один build runner выполняет последовательно: `make web-embed`; `(cd web && npm test && npm run build)`; `(cd server && go test ./... -count=1 && go vet ./...)`; `(cd server && go test -race ./internal/netpath ./internal/service ./internal/notifications ./internal/watchdapi ./internal/watchcompat ./internal/subwatch ./internal/monitor ./internal/bot ./internal/chatstore ./internal/vpnconfig ./internal/webapi ./internal/handler ./cmd/watchd ./cmd/bot -count=1)`; `bats router/test/*.bats router/test/unit router/test/integration`; `make -C server build`; capability smoke `(cd / && "$BUILD_ROOT/server/bin/telegram-bot" --watchd-capabilities)` без token/config; `make build-all`. `BUILD_ROOT` — собственная acceptance копия, созданная командой `BUILD_ROOT="$(mktemp -d /tmp/vpd-stage23-acceptance.XXXXXX)"` и заполненная `git archive HEAD | tar -x -C "$BUILD_ROOT"`; suite запускается из её корня. Перед удалением проверить владение этим точным каталогом. Expected: все команды exit 0, три daemon binaries для arm64/arm/mipsle. Known baseline findings/optional real-Xray skip перечислить явно; new tests проходят без credentials/network fixtures.
- [ ] **Browser acceptance:** После финальных изменений повторить end-to-end набор задачи 14 на двух viewports и всех shared pages, включая late responses, old watchd/new bot, controls при socket failure и восстановление. Отчёт включает фактические interactions/assertions; render screenshot служит приложением к проверке поведения.
- [ ] **Hardware gate:** Получить отдельное разрешение пользователя для Merlin/Keenetic действий. Проверить на реальном Xray (сначала подтвердить текущую версию; ранее установлена 26.2.6): RAM/CPU и traffic budget, bound WAN/tunnel DNS, main HTTPS/TPROXY и direct switch без client move, live/probe PID isolation, BusyBox/init/monit, socket filesystem и `/proc/PID/exe` capabilities, stop bot при продолжающемся failover, watchd restart с queue/pending restore, `/stop`, partial/first update и recovery running/stopped intent. Фиксировать измерения и ограничения; недоступная аппаратная проверка остаётся release gate.
- [ ] **Publish gate:** Перед push получить разрешение на deletion/publishing, сделать точный `git rm -- docs/superpowers/specs/2026-10-05-server-monitor-stages-2-3-design.md docs/superpowers/plans/2026-10-05-server-monitor-stages-2-3.md`; Commit subject `chore(docs): keep stage two and three plans in branch history`, body `Remove task planning documents from the published change while retaining them in feature branch history.` Проверить net diff к `master`: обе записи отсутствуют. Опубликовать в существующий PR #63 в пределах разрешения. Codex re-review запускается отдельным авторизованным `@codex review`, результат сверяется с новым commit SHA и inline comments. Закрыть свои dev/helper процессы и удалить только свои scratch-файлы.

## Self-Review Coverage

| Требование spec | Owning задачи |
|---|---|
| §1–3: единый PR, shared transport/fetch, watchd ownership, bot delivery | 1, 2, 5, 8 |
| §4: stop/disabled/unarmed, shutdown/restart, installed/running compatibility, update recovery | 6–8, 15, 16 |
| §5: fresh death/candidates/WAN/deadline, guarded direct switch, tunnel/legacy path, stable walk/return | 9–11 |
| §6: sole writer, atomic bounded store, recipients/history, ack/retry/storage failure | 3–5 |
| §7: subscription loss/recovery, indeterminate freeze, restart/rename/delete, atomic event+health | 12 |
| §8: unchanged monitor, new bounded IPC, watch states, UI/status/logs/manual availability | 3, 4, 8, 13, 14 |
| §9–10: constants, concurrency, race/Bats/UI/browser/architectures/hardware | Global Constraints, 1–16 |
| §11: план до реализации, local commits, exact cleanup и handoff | Global Constraints, 16 |

Проверены совпадение producer/consumer signatures, пять Review Focus случаев и отсутствие новых dependency/settings требований. Обязательные изменения ограничены согласованным дизайном; `pending_restore` закрывает обнаруженную при чтении кода RAM-only recovery границу. Перед выполнением пользователь проверяет этот механизм вместе с остальным планом и выбирает Subagent-driven или Native исполнение.
