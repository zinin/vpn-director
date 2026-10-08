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

✅ Done — see commit(s): `cfe679b`, `4aeb15e`

### Task 2: Сделать fetch подписок общим для бота и watchd

✅ Done — see commit(s): `7e1f98f`, `5c854c4`, `dad1f24`

### Task 3: Реализовать persistent notification store

✅ Done — see commit(s): `95c9f14`, `5339003`

### Task 4: Расширить локальный Unix API

✅ Done — see commit(s): `fb92335`

### Task 5: Подключить bot receiver и актуальных получателей

✅ Done — see commit(s): `0336fb8`, `8fcddf9`

### Task 6: Закрепить capabilities и mutation gate

✅ Done — see commit(s): `c600752`

### Task 7: Сохранить незавершённое восстановление клиентов

✅ Done — see commit(s): `cf85f71`, `b0b1b14`

### Task 8: Перенести watch lifecycle в watchd

✅ Done — see commit(s): `5146425`, `e5b0c5d`

### Task 9: Дать monitor проверяемое свидетельство свежего результата

✅ Done — see commit(s): `f454be3`, `3b70421`

### Task 10: Добавить раннее прямое переключение

✅ Done — see commit(s): `912c9eb`, `d32f0bd`, `16135aa`

### Task 11: Приоритизировать здоровье в обычном walk

✅ Done — see commit(s): `d2a7efb`

### Task 12: Публиковать переходы здоровья подписок

✅ Done — see commit(s): `089b228`

### Task 13: Показать watch отдельно в HTTP API и /status

✅ Done — see commit(s): `16c993e`, `5c854c4`, `dad1f24`

### Task 14: Отобразить состояния и проверить общие страницы в браузере

✅ Done — see commit(s): `4a5e152`, `dad1f24`

### Task 15: Закрепить установку, обновление и эксплуатационные правила

✅ Done — see commit(s): `ecaa35c`, `dad1f24`

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
