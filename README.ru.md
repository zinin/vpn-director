[🇬🇧 English](README.md) | 🇷🇺 Русский

# VPN Director для Asuswrt-Merlin и KeeneticOS

Выборочная маршрутизация трафика через Xray TPROXY и туннели OpenVPN/WireGuard.

## Возможности

- **Xray TPROXY**: Прозрачный прокси для выбранных LAN-клиентов через VLESS
- **Tunnel Director**: Маршрутизация трафика через OpenVPN/WireGuard по назначению
- **Маршрутизация по странам**: Направление трафика напрямую или через VPN в зависимости от географии назначения
- **Веб-интерфейс**: HTTPS веб-интерфейс для управления VPN Director из браузера
- **Telegram-бот**: Удалённое управление через Telegram (статус, настройка, перезапуск)
- **Простая установка**: Установка одной командой с интерактивной настройкой

## Быстрая установка

```bash
curl -fsSL \
  -H "Cache-Control: no-cache" \
  -H "Pragma: no-cache" \
  -H "If-Modified-Since: Thu, 01 Jan 1970 00:00:00 GMT" \
  "https://raw.githubusercontent.com/zinin/vpn-director/master/install.sh?v=$(date +%s)" \
| /opt/bin/bash
```

После установки:

1. Импортируйте VLESS-серверы (опционально):
   ```bash
   /opt/vpn-director/import_server_list.sh
   ```

2. Запустите мастер настройки:
   ```bash
   /opt/vpn-director/configure.sh
   ```

3. Откройте веб-интерфейс (устанавливается автоматически):
   ```
   https://<ip-роутера>:8444
   ```
   Merlin: пароль администратора роутера. KeeneticOS: пользователь `root` с паролем Entware.

4. Настройте Telegram-бота (опционально):
   ```bash
   /opt/vpn-director/setup_telegram_bot.sh
   ```

## Требования

### Asuswrt-Merlin

- Прошивка Asuswrt-Merlin
- Установленный Entware
- Необходимые пакеты:
  ```bash
  opkg install curl coreutils-base64 coreutils-sha256sum gawk jq xray-core procps-ng-pgrep procps-ng-pkill procps-ng-ps
  ```
- OpenVPN-клиент, настроенный в интерфейсе роутера (для Tunnel Director)

### KeeneticOS

- KeeneticOS 5.x (проверено на 5.1.5) с Entware на USB-накопителе
- Компонент прошивки «Модули ядра для Netfilter» (роутер один раз перезагружается при его добавлении)
- Пакеты, которые `install.sh` ставит по запросу (сам bash нужно установить заранее: `opkg install bash`):
  ```bash
  opkg install bash curl jq iptables ipset ip-full flock coreutils-nohup coreutils-base64 coreutils-sha256sum gawk procps-ng-pgrep procps-ng-pkill procps-ng-ps openssl-util cron xray
  ```
- Клиентский интерфейс OpenVPN или WireGuard, настроенный в интерфейсе роутера (для Tunnel Director)

Сборки для MIPS (`mipsle`) поставляются без проверки на устройстве.

Известное ограничение: список туннелей строится из всех интерфейсов OpenVPN и WireGuard на
роутере, включая OpenVPN- и WireGuard-**серверы**. Не назначайте клиентов на серверный
интерфейс: Tunnel Director направит их в серверный туннель самого роутера, где трафик будет
отброшен. Чтобы отличать серверы, нужен роутер с таким интерфейсом для проверки полей RCI; эта
проверка ещё не выполнена.

### Опционально

- `opkg install wget-ssl` — более быстрая и надёжная загрузка файлов зон стран (рекомендуется)
- `opkg install openssl-util` — для email-уведомлений
- `opkg install monit` — для автоматического перезапуска Xray при падении (см. [Мониторинг процессов](#мониторинг-процессов))
- `opkg install coreutils-tr` — исправляет баг команды `tr` (стандартный busybox `tr` портит символы при некоторых локалях)

## Ручная настройка

После установки конфигурационные файлы находятся:

- `/opt/vpn-director/vpn-director.json` — общая конфигурация (Xray + Tunnel Director)
- `/opt/etc/xray/config.json` — конфигурация сервера Xray

## Команды

```bash
# CLI VPN Director
/opt/vpn-director/vpn-director.sh status              # Показать статус
/opt/vpn-director/vpn-director.sh apply               # Применить конфигурацию
/opt/vpn-director/vpn-director.sh stop                # Остановить все компоненты
/opt/vpn-director/vpn-director.sh restart             # Перезапустить всё
/opt/vpn-director/vpn-director.sh update              # Обновить ipsets + применить

# Отдельные компоненты
/opt/vpn-director/vpn-director.sh status tunnel       # Только статус Tunnel Director
/opt/vpn-director/vpn-director.sh status ipset        # Только статус IPSet
/opt/vpn-director/vpn-director.sh restart xray        # Перезапустить только Xray TPROXY

# Опции (можно использовать с любой командой)
/opt/vpn-director/vpn-director.sh -v status           # Подробный вывод
/opt/vpn-director/vpn-director.sh -f apply            # Принудительное применение
/opt/vpn-director/vpn-director.sh --dry-run apply     # Показать, что будет сделано
/opt/vpn-director/vpn-director.sh --wait apply        # Ждать занятый лок до 120 с вместо пропуска
/opt/vpn-director/vpn-director.sh --unless-stopped apply  # Пропустить, если после этого был stop (так делает watch подписки в боте)

# Импорт серверов
/opt/vpn-director/import_server_list.sh
```

## Веб-интерфейс

HTTPS веб-интерфейс для управления VPN Director из браузера.

### Доступ

Откройте `https://<ip-роутера>:8444`.

- **Asuswrt-Merlin**: войдите с паролем администратора роутера (аутентификация через `/etc/shadow`).
- **KeeneticOS**: войдите как пользователь `root` с паролем Entware (`/opt/etc/passwd`, задаётся командой `passwd` по SSH).

Самоподписанный TLS-сертификат генерируется автоматически при установке. Браузер покажет предупреждение безопасности — это нормально.

### Возможности

| Вкладка | Описание |
|---------|----------|
| **Status** | Обзор состояния VPN Director |
| **Servers** | Управление серверами Xray, переключение активного сервера |
| **Clients** | Назначение маршрутов LAN-клиентам (пауза/возобновление/удаление) |
| **Exclusions** | Списки исключений по странам и IP/CIDR |
| **Logs** | Просмотр логов (бот, vpn, xray, webui) |
| **Settings** | Версия, самообновление, конфигурация |

### Конфигурация

Настройки веб-интерфейса находятся в `/opt/vpn-director/vpn-director.json` в секции `webui`:

```json
{
  "webui": {
    "port": 8444,
    "cert_file": "/opt/vpn-director/certs/server.crt",
    "key_file": "/opt/vpn-director/certs/server.key",
    "jwt_secret": "",
    "log_level": "info"
  }
}
```

`jwt_secret` генерируется автоматически при первом запуске, если оставлен пустым. `log_level` принимает `debug`, `info`, `warn`, `error` (по умолчанию `info`). Web UI пишет лог в `/tmp/vpn-director-webui.log`; все логи обрезаются при 200 КБ.

### Управление сервисом

```bash
/opt/etc/init.d/S98vpn-director-webui start
/opt/etc/init.d/S98vpn-director-webui stop
/opt/etc/init.d/S98vpn-director-webui restart
```

### Обновления

Вкладка **Settings** показывает текущую версию, последний релиз на GitHub и его changelog. Кнопка «Update to vX» скачивает релиз и обновляет и Web UI, и Telegram-бота, перезапуская те, что работали; страница опрашивает версию и сама перезагружается, когда новая сборка поднялась. Сессия входа переживает обновление.

Об обновлении, запущенном из Web UI, бот сообщает в Telegram во все активные чаты. Команда `/update` в боте делает то же самое с другой стороны — оба пути обновляют оба демона.

Целостность обновления обеспечивается только TLS-соединением с github.com: ни бинарники, ни скрипты не подписаны и не проверяются контрольной суммой, а устанавливаются и запускаются от root. Это та же модель доверия, что и у команды быстрой установки `curl … | bash` выше: тот, кто может опубликовать релиз в этом репозитории, может выполнить код на вашем роутере.

> Переход **на** первый релиз с единым обновлением выполняет прежний обновлятор бота, который не знает про Web UI. После этого перехода один раз повторите команду [быстрой установки](#быстрая-установка); все последующие обновления охватывают оба демона.

> **Обновление с v0.11.x и более ранних версий.** Встроенный в те релизы обновлятор скачивает фиксированный список файлов без `lib/platform.sh`, поэтому кнопка «Update» устанавливает этот релиз не полностью: shell-CLI, хуки firewall и ежедневное обновление перестают работать (правила Xray TPROXY и Tunnel Director больше не восстанавливаются после перезапуска firewall), пока вы один раз не выполните команду [быстрой установки](#быстрая-установка). Сами Web UI и бот продолжают работать. Все последующие обновления читают манифест релиза, и этот шаг им не нужен.

## Telegram-бот

Удалённое управление через Telegram с авторизацией по имени пользователя.

### Настройка

1. Создайте бота через [@BotFather](https://t.me/BotFather) и получите токен
2. Запустите скрипт настройки:
   ```bash
   /opt/vpn-director/setup_telegram_bot.sh
   ```
3. Введите токен бота и разрешённые имена пользователей (без @)

### Команды бота

| Команда | Описание |
|---------|----------|
| `/status` | Статус VPN Director |
| `/xray` | Переключение сервера Xray |
| `/servers` | Список серверов |
| `/import <url>` | Импорт VLESS-подписки (авто-синхронизация xray.servers) |
| `/exclude` | Управление исключёнными IP/CIDR |
| `/clients` | Управление VPN-клиентами |
| `/configure` | Мастер настройки |
| `/restart` | Перезапустить VPN Director |
| `/stop` | Остановить VPN Director |
| `/logs [bot\|vpn\|xray\|webui\|all] [N]` | Последние логи (по умолчанию: all, 20 строк) |
| `/ip` | Внешний IP |
| `/update` | Обновить до последней версии |
| `/version` | Версия бота |

### Мастер настройки

Команда `/configure` запускает 4-шаговый мастер:
1. Выбор сервера Xray
2. Исключение из прокси (коды стран, IP/CIDR)
3. Настройка LAN-клиентов с маршрутизацией (Xray/OpenVPN/WireGuard)
4. Проверка и применение

## Как это работает

### Xray TPROXY

Трафик от указанных LAN-клиентов прозрачно перенаправляется через Xray с помощью TPROXY. Прокси использует протокол VLESS поверх TLS для подключения к вашему VPN-серверу.

### Tunnel Director

Маршрутизирует трафик от указанных LAN-клиентов через туннели OpenVPN/WireGuard в зависимости от назначения. Настраиваемые исключения позволяют направлять трафик к выбранным странам напрямую для оптимальной производительности. Ключ туннеля — идентификатор из списка платформы (`wgc1` / `ovpnc1` на Merlin, `OpenVPN0` / `Wireguard1` на KeeneticOS). У OpenVPN-туннеля можно задать необязательный `gateway` (next hop; иначе берётся первый хост подсети). WireGuard его игнорирует.

```json
{
  "tunnel_director": {
    "tunnels": {
      "wgc1": { "clients": ["192.168.50.0/24"], "exclude": ["<country_code>"] },
      "OpenVPN0": {
        "clients": ["192.168.1.5"],
        "exclude": ["<country_code>"],
        "gateway": "10.73.149.1"
      }
    }
  }
}
```

### IPSet по странам

Списки IP-адресов стран загружаются автоматически из нескольких источников с резервным переключением:
1. GeoLite2 через GitHub (firehol/blocklist-ipsets) — наиболее точный
2. IPDeny через зеркало на GitHub — не заблокирован в большинстве регионов
3. IPDeny напрямую — может быть заблокирован в некоторых регионах
4. Ручной ввод — интерактивный запрос, если все источники недоступны

## Скрипты автозапуска

Проект использует Entware init.d для автоматического запуска:

| Скрипт | Когда вызывается | Назначение |
|--------|------------------|------------|
| `/opt/etc/init.d/S99vpn-director` | После инициализации Entware | Запускает `vpn-director.sh apply` для инициализации всех компонентов |
| `/jffs/scripts/firewall-start` | После применения правил файрвола (Merlin) | Повторно применяет конфигурацию после перезагрузки файрвола |
| `/jffs/scripts/wan-event` | При подключении WAN (Merlin) | Запускает `vpn-director.sh apply` при подключении WAN |
| `/opt/etc/ndm/netfilter.d`, `wan.d`, `iflayerchanged.d` (`50-vpn-director.sh`) | Пересборка файрвола NDM / старт WAN / IPv4-слой VPN-клиента (KeeneticOS) | Отсоединённый `vpn-director.sh --wait apply` |

**Примечание:** Скрипт init.d проверяет доступность bash из Entware перед запуском скриптов vpn-director.

На Merlin, для включения пользовательских скриптов: Administration -> System -> Enable JFFS custom scripts and configs -> Yes

## Мониторинг процессов

Xray, Telegram-бот и веб-интерфейс могут иногда падать. Используйте monit для автоматического перезапуска.

### Настройка

1. Установите monit:
   ```bash
   opkg install monit
   ```

2. Создайте конфигурации в `/opt/etc/monit.d/`:

   **xray:**
   ```
   check process xray matching "xray"
       start program = "/opt/etc/init.d/S24xray start"
       stop program = "/opt/etc/init.d/S24xray stop"
       if does not exist then restart
   ```

   **telegram-bot:**
   ```
   check process telegram-bot matching "telegram-bot"
       start program = "/opt/etc/init.d/S98telegram-bot start"
       stop program = "/opt/etc/init.d/S98telegram-bot stop"
       if does not exist then restart
   ```

   **webui:**
   ```
   check process webui matching "webui"
       start program = "/opt/etc/init.d/S98vpn-director-webui start"
       stop program = "/opt/etc/init.d/S98vpn-director-webui stop"
       if does not exist then restart
   ```

3. Включите директорию конфигов в `/opt/etc/monitrc`:
   ```
   include /opt/etc/monit.d/*
   ```

4. Отредактируйте `/opt/etc/monitrc`, установите интервал проверки:
   ```
   set daemon 30    # проверка каждые 30 секунд
   ```

5. Перезапустите monit:
   ```bash
   /opt/etc/init.d/S99monit restart
   ```

6. Проверьте:
   ```bash
   monit status
   ```

## Лицензия

Copyright (C) 2026 Alexander Zinin <mail@zinin.ru>

Лицензировано под GNU Affero General Public License v3.0 или более поздней версии
(AGPL-3.0-or-later). См. `LICENSE`.
