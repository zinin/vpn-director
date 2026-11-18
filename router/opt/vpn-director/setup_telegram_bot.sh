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

# bot_config_write <file> <token> <users_json>
#   Writes the bot's config. Its token drives the bot, which manages the router as root, so the
#   file is 0600 from the moment it exists: a temp file beside <file>, which mktemp makes 0600,
#   renamed into place. The rename gives it a new inode, so a 0666 file an earlier release left
#   does not keep its mode. rc 1 - the temp file removed, <file> left as it was - when a step fails.
bot_config_write() {
    local file="$1" token="$2" users="$3" tmp
    tmp=$(mktemp "$file.XXXXXX") || return 1
    if ! jq -n --arg token "$token" --argjson users "$users" \
            '{bot_token: $token, allowed_users: $users, log_level: "info", update_check_interval: "24h"}' \
            > "$tmp" ||
        ! chmod 600 "$tmp" || ! mv -f "$tmp" "$file"; then
        rm -f "$tmp"
        return 1
    fi
}

main() {
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

    if ! bot_config_write "$CONFIG_FILE" "$BOT_TOKEN" "$USERS_JSON"; then
        echo "Error: could not write $CONFIG_FILE"
        exit 1
    fi

    echo
    echo "Config created: $CONFIG_FILE"

    # Restart bot via init script
    if [[ -x /opt/etc/init.d/S98telegram-bot ]]; then
        /opt/etc/init.d/S98telegram-bot restart
    fi

    echo
    echo "The bot finds its own way to api.telegram.org: direct, then Xray SOCKS, then a VPN Director tunnel"
    echo "Done! Send /start to the bot"
}

if [[ ${1:-} == "--source-only" ]]; then
    # shellcheck disable=SC2317
    return 0 2>/dev/null || exit 0
fi

main "$@"
