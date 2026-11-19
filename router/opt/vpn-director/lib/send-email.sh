#!/bin/sh
# shellcheck shell=bash
# KeeneticOS has no /usr/bin/env and mounts / read-only, so the usual
# "#!/usr/bin/env bash" cannot start this script there. Begin as a POSIX shell
# and hand over to bash by absolute path - Entware's first, a workstation's
# after it. PATH is no help here: Asuswrt-Merlin's /bin/sh has no "command"
# builtin (see .claude/rules/shell-conventions.md) and its /bin/bash is a
# symlink to busybox rather than bash.

# -----------------------------------------------------------------------------
# Disable unneeded shellcheck warnings
# -----------------------------------------------------------------------------
# shellcheck disable=SC1090
# shellcheck disable=SC2086
# shellcheck disable=SC2154
# These stay above the hand-over block: shellcheck reads a directive as
# file-wide only while no command has run yet.

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

###############################################################################
# send_email.sh - lightweight email notification helper for Asuswrt-Merlin
# -----------------------------------------------------------------------------
# What it does:
#   * Sends a single email using the router's amtm email configuration.
#   * Concatenates all body arguments with spaces into the final message body.
#
# Usage:
#   send_email.sh "<subject>" "<body part 1>" [<body part 2> ...]
#
# Requirements:
#   * amtm email must be configured on the router beforehand.
###############################################################################

# -----------------------------------------------------------------------------
# Abort script on any error
# -----------------------------------------------------------------------------
set -euo pipefail

###############################################################################
# 0a. Load utils
###############################################################################
. /opt/vpn-director/lib/common.sh

###############################################################################
# 0a'. Platforms without amtm email have nothing to send
###############################################################################
if [[ "$(platform_email_supported)" != "1" ]]; then
    log -l DEBUG "Email notifications are not supported on this platform; skipping"
    exit 0
fi

###############################################################################
# 0b. Define constants
###############################################################################
AMTM_EMAIL_DIR="/jffs/addons/amtm/mail"
AMTM_EMAIL_CONF="$AMTM_EMAIL_DIR/email.conf"
AMTM_EMAIL_PW_ENC="$AMTM_EMAIL_DIR/emailpw.enc"

# Wait time (seconds) before retrying email after network failure
RETRY_DELAY=60

###############################################################################
# 0c. Ensure email is configured
###############################################################################
if [[ ! -r "$AMTM_EMAIL_CONF" ]] || [[ ! -r "$AMTM_EMAIL_PW_ENC" ]]; then
    log -l ERROR "Email is not configured in amtm. Please configure it first"
    exit 1
fi

# Load amtm variables:
#   SMTP       - mail server host
#   PORT       - mail server port
#   PROTOCOL   - "smtp" or "smtps"
#   SSL_FLAG   - none or "--insecure"
#   emailPwEnc - encrypted password
#   TO_NAME, TO_ADDRESS, FROM_ADDRESS, USERNAME
. "$AMTM_EMAIL_CONF"

###############################################################################
# 0d. Parse args & define variables
###############################################################################
SUBJECT="${1-}"                       # first argument = email subject
shift                                 # drop $1, so $@ now starts with the body

# Remaining arguments = message body.
# Join them with spaces and translate \n etc. using printf '%b'
BODY=$(printf '%b' "$*")

# Validate subject
if [[ -z "${SUBJECT//[[:space:]]/}" ]]; then
    log -l ERROR "Email subject is empty. Please provide an argument"
    exit 2
fi

# Validate body
if [[ -z "${BODY//[[:space:]]/}" ]]; then
    log -l ERROR "Email body is empty. Please provide at least one body argument"
    exit 2
fi

# Decrypt password
PASSWORD="$(/usr/sbin/openssl aes-256-cbc "$emailPwEnc" \
    -d -in "$AMTM_EMAIL_PW_ENC" -pass pass:ditbabot,isoi 2>/dev/null)"

###############################################################################
# 1. Build the message
###############################################################################
TMP_MAIL=$(tmp_file)

FROM_NAME="ASUS $(platform_model || true)"   # router name shown in "From:"

{
    printf 'From: "%s"<%s>\n'       "$FROM_NAME" "$FROM_ADDRESS"
    printf 'To: "%s"<%s>\n'         "$TO_NAME" "$TO_ADDRESS"
    printf 'Subject: %s\n'          "$SUBJECT"
    printf 'Date: %s\n'             "$(date -R)"
    printf '\nHey there,\n\n%s\n\n' "$BODY"
    printf '%s\n' '--------------------'
    printf 'Best regards,\nYour friendly router\n'
} > "$TMP_MAIL"

###############################################################################
# 2. Send over SMTP using curl
###############################################################################
for try in 1 2 3; do
    if /usr/sbin/curl -sS --url "${PROTOCOL}://${SMTP}:${PORT}" \
            --mail-from "$FROM_ADDRESS" \
            --mail-rcpt "$TO_ADDRESS" \
            --upload-file "$TMP_MAIL" \
            --ssl-reqd \
            --crlf \
            --user "$USERNAME:$PASSWORD" \
            $SSL_FLAG;
    then
        log "Email sent to $TO_ADDRESS: $SUBJECT"
        break
    fi

    if [[ "$try" -lt 3 ]]; then
        log -l WARN "Email send failed, retrying in ${RETRY_DELAY}s... (attempt $try/3)"
        sleep "$RETRY_DELAY"
    else
        log -l ERROR "Failed to send email to $TO_ADDRESS: $SUBJECT after 3 attempts"
        exit 3
    fi
done
