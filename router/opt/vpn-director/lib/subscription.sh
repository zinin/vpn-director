#!/usr/bin/env bash
###############################################################################
# lib/subscription.sh - read a VPN subscription into servers that carry a
# ready Xray outbound: share links (vless, vmess, trojan, ss, hysteria2/hy2),
# base64 or plain, or Xray JSON.
#
# The Go importer does the same in server/internal/subscription, and both
# answer to the cases in testdata/subscription at the repository root:
# router/test/unit/subscription.bats runs them here, the Go tests there. A rule
# changed on one side only fails one of the two suites.
#
# subscription_decode reads the body on stdin and prints
#   {"total": N, "servers": [{name, address, port, outbound}],
#    "skipped": [{name, reason, detail}]}
# For a body it cannot read at all it prints the reason on stderr - "empty
# subscription", "invalid JSON subscription" or "unrecognized subscription
# format" - and returns 1.
#
# Entware's jq is built without oniguruma, so the jq here never uses test,
# match, capture, scan, splits, sub, gsub or a two-argument split: bash and
# gawk do the pattern work.
###############################################################################

_SUB_SPACE=$' \t\r\n\v\f'

# _sub_unescape <value> <plus>: decodes %XX into _SUB_U; + is a space when plus
# is 1. A % without two hex digits after it, or %00, returns 1 - unescape in
# server/internal/subscription/text.go.
_sub_unescape() {
    local s=$1 bs='\x'
    if [[ $2 == 1 ]]; then
        s=${s//+/ }
    fi
    if [[ $s != *%* ]]; then
        _SUB_U=$s
        return 0
    fi
    local rest=${s//%[0-9A-Fa-f][0-9A-Fa-f]/}
    if [[ $rest == *%* || $s == *%00* ]]; then
        return 1
    fi
    s=${s//\\/\\\\}
    printf -v _SUB_U '%b' "${s//%/$bs}"
}

# _sub_b64 <text>: decodes base64 of either alphabet, padded or not, ignoring
# whitespace; NUL bytes are dropped. Returns 1 for anything else.
_sub_b64() {
    local s=$1 pad
    s=${s//[$_SUB_SPACE]/}
    s=${s//-/+}
    s=${s//_/\/}
    while [[ $s == *= ]]; do
        s=${s%=}
    done
    [[ -n $s && $s =~ ^[A-Za-z0-9+/]+$ ]] || return 1
    case $(( ${#s} % 4 )) in
        1) return 1 ;;
        2) pad='==' ;;
        3) pad='=' ;;
        *) pad='' ;;
    esac
    printf '%s%s' "$s" "$pad" | base64 -d 2>/dev/null | tr -d '\000'
}

# _sub_trim <text>: prints text without ASCII whitespace at either end.
_sub_trim() {
    local s=$1
    s=${s#"${s%%[!$_SUB_SPACE]*}"}
    printf '%s' "${s%"${s##*[!$_SUB_SPACE]}"}"
}

# _sub_clean_names: "<decode>\t<raw>" lines in, one JSON string per line out.
# The filter of cleanName in server/internal/subscription/names.go, byte for
# byte under LC_ALL=C: ASCII letters, digits, space and .,;:!?()-, and every
# two-byte UTF-8 character; a lead byte without its continuation bytes goes
# alone. With decode 1 the raw name is first percent-decoded leniently, + as
# a space and %00 as nothing. What it keeps holds no quote and no backslash,
# so a pair of quotes around it is a JSON string.
_sub_clean_names() {
    LC_ALL=C gawk '
        function cont(s, i, k,   j, b) {
            for (j = 1; j <= k; j++) {
                b = ord[substr(s, i + j, 1)] + 0
                if (b < 128 || b > 191) return 0
            }
            return 1
        }
        function hexval(h) { return index("0123456789abcdef", tolower(h)) - 1 }
        BEGIN { for (i = 1; i < 256; i++) ord[sprintf("%c", i)] = i }
        {
            s = substr($0, 3); n = length(s)
            if (substr($0, 1, 1) == "1") {
                t = ""
                for (i = 1; i <= n; i++) {
                    c = substr(s, i, 1)
                    if (c == "%" && i + 2 <= n && substr(s, i + 1, 2) ~ /^[0-9A-Fa-f][0-9A-Fa-f]$/) {
                        v = hexval(substr(s, i + 1, 1)) * 16 + hexval(substr(s, i + 2, 1))
                        if (v != 0) t = t sprintf("%c", v)
                        i += 2
                    } else if (c == "+") t = t " "
                    else t = t c
                }
                s = t; n = length(s)
            }
            out = ""
            for (i = 1; i <= n; ) {
                c = substr(s, i, 1); b = ord[c] + 0
                if (c ~ /[A-Za-z0-9 .,;:!?()-]/) { out = out c; i++ }
                else if (b >= 194 && b <= 223 && cont(s, i, 1)) { out = out substr(s, i, 2); i += 2 }
                else if (b >= 224 && b <= 239 && cont(s, i, 2)) i += 3
                else if (b >= 240 && b <= 244 && cont(s, i, 3)) i += 4
                else i++
            }
            gsub(/ +/, " ", out); sub(/^[ ,]+/, "", out); sub(/[ ,]+$/, "", out)
            printf "\"%s\"\n", out
        }'
}
