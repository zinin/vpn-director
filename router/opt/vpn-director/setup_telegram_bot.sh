#!/bin/sh
# shellcheck shell=bash
# KeeneticOS has no /usr/bin/env and mounts / read-only, so the usual
# "#!/usr/bin/env bash" cannot start this script there. Begin as a POSIX shell
# and hand over to bash by absolute path - Entware's first, a workstation's
# after it. PATH is no help here: Asuswrt-Merlin's /bin/sh has no "command"
# builtin (see .claude/rules/shell-conventions.md) and its /bin/bash is a
# symlink to busybox rather than bash.
# Asuswrt-Merlin runs everything under umask 0; nothing this creates may be writable by others.
umask 022
if [ -z "${BASH_VERSION:-}" ]; then
    for _vpd_bash in /opt/bin/bash /usr/bin/bash /bin/bash; do
        # Asuswrt-Merlin's /bin/bash is busybox: a POSIX shell that never sets
        # BASH_VERSION, so exec-ing it would re-run this block forever. Only a
        # candidate that proves it is bash gets the script.
        [ -x "$_vpd_bash" ] && "$_vpd_bash" -c '[ -n "$BASH_VERSION" ]' 2>/dev/null &&
            exec "$_vpd_bash" "$0" "$@"
    done
    echo "$0: bash not found; install it (Entware package \"bash\")" >&2
    exit 1
fi
set -euo pipefail

VPD_DIR="/opt/vpn-director"
CONFIG_FILE="$VPD_DIR/telegram-bot.json"

echo "Telegram Bot Setup"
echo "=================="
echo

# Check for jq
if ! command -v jq &> /dev/null; then
    echo "Error: jq not installed. Install via opkg install jq"
    exit 1
fi

# Bot token
printf "Enter bot token: "
read -r BOT_TOKEN < /dev/tty

if [[ -z "$BOT_TOKEN" ]]; then
    echo "Error: token cannot be empty"
    exit 1
fi

# Users
USERS=()
while true; do
    printf "Enter username (without @): "
    read -r USERNAME < /dev/tty

    if [[ -n "$USERNAME" ]]; then
        USERS+=("$USERNAME")
    fi

    printf "Add another? [y/N]: "
    read -r REPLY < /dev/tty
    case "$REPLY" in
        [Yy]*) continue ;;
        *) break ;;
    esac
done

if [[ ${#USERS[@]} -eq 0 ]]; then
    echo "Error: add at least one user"
    exit 1
fi

# Create JSON
USERS_JSON=$(printf '%s\n' "${USERS[@]}" | jq -R . | jq -s .)

jq -n \
    --arg token "$BOT_TOKEN" \
    --argjson users "$USERS_JSON" \
    '{bot_token: $token, allowed_users: $users, log_level: "info", update_check_interval: "24h"}' > "$CONFIG_FILE"

echo
echo "Config created: $CONFIG_FILE"

# Restart bot via init script
if [[ -x /opt/etc/init.d/S98telegram-bot ]]; then
    /opt/etc/init.d/S98telegram-bot restart
fi

echo
echo "The bot finds its own way to api.telegram.org: direct, then Xray SOCKS, then a VPN Director tunnel"
echo "Done! Send /start to the bot"
