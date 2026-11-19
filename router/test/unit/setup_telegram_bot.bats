#!/usr/bin/env bats

# setup_telegram_bot.sh asks for the bot token and the users, then writes
# telegram-bot.json. The token lets anyone holding it drive the bot, which manages the router as
# root, so the file is 0600 from the moment it is written - under Asuswrt-Merlin's umask 0 too,
# and over a 0666 file an earlier release left.

load '../test_helper'

setup() {
    source "$SCRIPTS_DIR/setup_telegram_bot.sh" --source-only
    umask 0    # Asuswrt-Merlin's
    CONFIG="$BATS_TEST_TMPDIR/telegram-bot.json"
}

@test "bot_config_write: the config is 0600 under a caller's umask 0" {
    bot_config_write "$CONFIG" "123:abc" '["alice"]'
    assert_equal "$(stat -c %a "$CONFIG")" 600
    assert_equal "$(jq -c . "$CONFIG")" '{"bot_token":"123:abc","allowed_users":["alice"],"log_level":"info","update_check_interval":"24h"}'
}

@test "bot_config_write: a 0666 config from before is replaced by a 0600 one" {
    printf '{}\n' > "$CONFIG"
    chmod 666 "$CONFIG"
    bot_config_write "$CONFIG" "123:abc" '["alice"]'
    assert_equal "$(stat -c %a "$CONFIG")" 600
}

@test "bot_config_write: a failed write keeps the old config and leaves no temp file" {
    printf '{"old":1}\n' > "$CONFIG"
    run bot_config_write "$CONFIG" "123:abc" 'not json'
    assert_failure
    assert_equal "$(cat "$CONFIG")" '{"old":1}'
    run compgen -G "$CONFIG.*"
    assert_failure
}

# mv -f into a directory moves the file inside it: a directory at the config path would take the
# config, and the wizard would report it created and restart the bot without one.
@test "bot_config_write: a directory at the config path is a failure, and nothing is written in or beside it" {
    mkdir "$CONFIG"
    run bot_config_write "$CONFIG" "123:abc" '["alice"]'
    assert_failure
    run compgen -G "$CONFIG/*"
    assert_failure
    run compgen -G "$CONFIG.*"
    assert_failure
}

# /proc/<pid>/cmdline is readable by every process on the router, the `nobody` ones among them: a
# token on jq's command line would be theirs for as long as jq runs. /proc/<pid>/environ is root's.
@test "bot_config_write: the token never reaches jq's command line" {
    local real_jq
    real_jq=$(command -v jq)
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    cat > "$BATS_TEST_TMPDIR/bin/jq" <<RECORDER
#!/bin/sh
printf '%s\n' "\$@" >> "$BATS_TEST_TMPDIR/jq.argv"
exec "$real_jq" "\$@"
RECORDER
    chmod +x "$BATS_TEST_TMPDIR/bin/jq"

    PATH="$BATS_TEST_TMPDIR/bin:$PATH" bot_config_write "$CONFIG" "123:secret-token" '["alice"]'

    assert_equal "$(jq -r .bot_token "$CONFIG")" "123:secret-token"
    [[ -s "$BATS_TEST_TMPDIR/jq.argv" ]] || fail "jq was not called through the recorder"
    run grep -c 'secret-token' "$BATS_TEST_TMPDIR/jq.argv"
    assert_output "0"
}
