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

declare -gA _SUB_Q=() _SUB_V=()
declare -ga _SUB_RECORDS=() _SUB_RAWNAMES=()

# The jq variables a record is built from. jq refuses a program that names a
# variable it was not given, so every one of them is always passed.
_SUB_VARS="net sec sni fp alpn pcs vcn pbk sid spx pqv path host serviceName authority mode extra
flow user enc addr method password pin obfs obfspw"

# The query keys a converter reads; the rest of a query is never stored.
_SUB_KEYS=" type security encryption flow sni fp alpn pbk sid spx pqv host path
headerType serviceName mode authority extra pcs vcn allowInsecure insecure
plugin obfs obfs-password pinSHA256 "

# jq shared by every converter and by the final assembly.
# shellcheck disable=SC2016  # $vars are jq's, set with --arg
_SUB_JQ_LIB='
def trimsp: if startswith(" ") then .[1:] | trimsp elif endswith(" ") then .[:-1] | trimsp else . end;
def list: split(",") | map(trimsp);
def prune: walk(if type == "object" then with_entries(select(.value != "" and .value != [] and .value != {}))
                elif type == "array" then map(select(. != "" and . != [] and . != {}))
                else . end);
def stream:
  {network: $net, security: $sec}
  + (if $sec == "tls" then {tlsSettings: {serverName: $sni, fingerprint: $fp, alpn: ($alpn | list),
                                          pinnedPeerCertSha256: $pcs, verifyPeerCertByName: $vcn}}
     elif $sec == "reality" then {realitySettings: {serverName: $sni, fingerprint: $fp, publicKey: $pbk,
                                                    shortId: $sid, spiderX: $spx, mldsa65Verify: $pqv}}
     else {} end)
  + (if $net == "ws" then {wsSettings: {path: $path, host: $host}}
     elif $net == "httpupgrade" then {httpupgradeSettings: {path: $path, host: $host}}
     elif $net == "grpc" then {grpcSettings: ({serviceName: $serviceName, authority: $authority}
                                              + (if $mode == "multi" then {multiMode: true} else {} end))}
     elif $net == "xhttp" then {xhttpSettings: ({path: $path, host: $host, mode: $mode}
                                                + (if $extra == "" then {} else {extra: ($extra | fromjson)} end))}
     else {} end);
'

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

# _sub_skip <reason> <detail>: marks the entry skipped.
_sub_skip() {
    _SUB_REASON=$1
    _SUB_DETAIL=$2
}

# _sub_query <query>: fills _SUB_Q with the keys a converter reads, the first
# value of a key winning; returns 1 on a bad escape anywhere in the query.
_sub_query() {
    _SUB_Q=()
    local q=$1 part key val
    while [[ -n $q ]]; do
        part=${q%%&*}
        if [[ $part == "$q" ]]; then
            q=""
        else
            q=${q#*&}
        fi
        [[ -n $part ]] || continue
        key=${part%%=*}
        val=""
        if [[ $part == *=* ]]; then
            val=${part#*=}
        fi
        _sub_unescape "$key" 1 || return 1
        key=$_SUB_U
        _sub_unescape "$val" 1 || return 1
        val=$_SUB_U
        if [[ $_SUB_KEYS == *[[:space:]]"$key"[[:space:]]* && -z ${_SUB_Q[$key]+set} ]]; then
            _SUB_Q[$key]=$val
        fi
    done
}

# _sub_hostport <host[:port]|[ipv6][:port]>: sets _SUB_HOST (brackets
# removed), _SUB_RAWPORT and _SUB_HASPORT.
_sub_hostport() {
    local hp=$1 tail
    _SUB_HOST="" _SUB_RAWPORT="" _SUB_HASPORT=0
    if [[ $hp == \[* ]]; then
        if [[ $hp != *\]* ]]; then
            _sub_skip invalid "bad IPv6 address"
            return 1
        fi
        _SUB_HOST=${hp#\[}
        _SUB_HOST=${_SUB_HOST%%\]*}
        tail=${hp#*\]}
        if [[ -n $tail ]]; then
            if [[ $tail != :* ]]; then
                _sub_skip invalid "bad address"
                return 1
            fi
            _SUB_RAWPORT=${tail#:}
            _SUB_HASPORT=1
        fi
    elif [[ $hp == *:* ]]; then
        _SUB_HOST=${hp%:*}
        _SUB_RAWPORT=${hp##*:}
        _SUB_HASPORT=1
    else
        _SUB_HOST=$hp
    fi
    if [[ -z $_SUB_HOST ]]; then
        _sub_skip invalid "missing host"
        return 1
    fi
}

# _sub_port <raw>: one to five digits, 1-65535, into _SUB_PORT.
_sub_port() {
    if [[ $1 =~ ^[0-9]{1,5}$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 )); then
        _SUB_PORT=$((10#$1))
        return 0
    fi
    _sub_skip invalid "bad port \"$1\""
    return 1
}

# _sub_link <rest>: the grammar of spec 6.2 up to the host -
# scheme://userinfo@host[:port][/][?query][#fragment]. Sets _SUB_RAWNAME,
# _SUB_USER (decoded, + kept), the host fields and _SUB_RAWQUERY.
_sub_link() {
    local rest=$1 body main authority
    body=${rest%%#*}
    if [[ $body != "$rest" ]]; then
        _SUB_RAWNAME=${rest#*#}
    fi
    main=${body%%\?*}
    _SUB_RAWQUERY=""
    if [[ $main != "$body" ]]; then
        _SUB_RAWQUERY=${body#*\?}
    fi
    authority=${main%%/*}
    if [[ $authority != *@* ]]; then
        _sub_skip invalid "missing userinfo"
        return 1
    fi
    if ! _sub_unescape "${authority%@*}" 0; then
        _sub_skip invalid "userinfo: bad percent escape"
        return 1
    fi
    _SUB_USER=$_SUB_U
    if [[ -z $_SUB_USER ]]; then
        _sub_skip invalid "missing userinfo"
        return 1
    fi
    _sub_hostport "${authority##*@}"
}

# _sub_port_query: the port a scheme requires, then the query.
_sub_port_query() {
    if [[ $_SUB_HASPORT != 1 ]]; then
        _sub_skip invalid "missing port"
        return 1
    fi
    _sub_port "$_SUB_RAWPORT" || return 1
    if ! _sub_query "$_SUB_RAWQUERY"; then
        _sub_skip invalid "query: bad percent escape"
        return 1
    fi
}

_sub_truthy() {
    [[ $1 == 1 || $1 == true ]]
}

# _sub_stream <default security>: the checks of spec 6.3, in its order. Sets
# the _SUB_V entries the jq stream function reads.
_sub_stream() {
    local net=${_SUB_Q[type]:-} sec=${_SUB_Q[security]:-} ht=${_SUB_Q[headerType]:-} k
    case $net in
        ''|tcp|raw) net=tcp ;;
        ws|websocket) net=ws ;;
        grpc|httpupgrade) ;;
        xhttp|splithttp) net=xhttp ;;
        *)
            _sub_skip unsupported "transport $net"
            return 1
            ;;
    esac
    if [[ $net == tcp && -n $ht && $ht != none ]]; then
        _sub_skip unsupported "tcp header $ht"
        return 1
    fi
    [[ -n $sec ]] || sec=$1
    case $sec in
        none) ;;
        tls)
            if { _sub_truthy "${_SUB_Q[allowInsecure]:-}" || _sub_truthy "${_SUB_Q[insecure]:-}"; } &&
                [[ -z ${_SUB_Q[pcs]:-} ]]; then
                _sub_skip unsupported "insecure TLS"
                return 1
            fi
            ;;
        reality)
            if [[ -z ${_SUB_Q[pbk]:-} || -z ${_SUB_Q[sni]:-} || -z ${_SUB_Q[fp]:-} ]]; then
                _sub_skip invalid "reality needs pbk, sni and fp"
                return 1
            fi
            ;;
        *)
            _sub_skip unsupported "security $sec"
            return 1
            ;;
    esac
    if [[ $net == xhttp && -n ${_SUB_Q[extra]:-} ]] &&
        ! jq -es 'length == 1 and (.[0] | type) == "object"' <<< "${_SUB_Q[extra]}" >/dev/null 2>&1; then
        _sub_skip invalid "xhttp extra is not a JSON object"
        return 1
    fi
    _SUB_V[net]=$net
    _SUB_V[sec]=$sec
    for k in sni fp alpn pcs vcn pbk sid spx pqv path host serviceName authority mode extra flow; do
        _SUB_V[$k]=${_SUB_Q[$k]:-}
    done
}

# _sub_record <jq outbound>: _SUB_REC, the record of a converted server.
_sub_record() {
    local -a args=()
    local k
    _SUB_V[addr]=$_SUB_HOST
    for k in $_SUB_VARS; do
        args+=(--arg "$k" "${_SUB_V[$k]:-}")
    done
    _SUB_REC=$(jq -cn "${args[@]}" --argjson port "$_SUB_PORT" \
        "$_SUB_JQ_LIB"' {address: $addr, port: $port, outbound: ('"$1"' | prune)}')
}

_sub_vless() {
    _sub_link "$1" || return 1
    _sub_port_query || return 1
    _sub_stream none || return 1
    _SUB_V[user]=$_SUB_USER
    _SUB_V[enc]=${_SUB_Q[encryption]:-none}
    _sub_record '{protocol: "vless", settings: {vnext: [{address: $addr, port: $port,
        users: [{id: $user, encryption: $enc, flow: $flow}]}]}, streamSettings: stream}'
}

# Trojan runs over TLS, so a link that names no security gets tls.
_sub_trojan() {
    _sub_link "$1" || return 1
    _sub_port_query || return 1
    _sub_stream tls || return 1
    _SUB_V[user]=$_SUB_USER
    _sub_record '{protocol: "trojan", settings: {servers: [{address: $addr, port: $port,
        password: $user}]}, streamSettings: stream}'
}

_sub_vmess_record() {
    _sub_record '{protocol: "vmess", settings: {vnext: [{address: $addr, port: $port,
        users: [{id: $user, security: $enc}]}]}, streamSettings: stream}'
}

# Both vmess forms: the URL form, recognized by an @, and the v2rayN form,
# base64 of a JSON object. Xray speaks VMess AEAD only; alterId is ignored.
_sub_vmess() {
    local body=${1%%#*} text fields k vn
    if [[ $body == *@* ]]; then
        _sub_link "$1" || return 1
        _sub_port_query || return 1
        _sub_stream none || return 1
        _SUB_V[user]=$_SUB_USER
        _SUB_V[enc]=${_SUB_Q[encryption]:-auto}
        _sub_vmess_record
        return
    fi
    _SUB_DECODE=0
    if ! text=$(_sub_b64 "$body") ||
        ! fields=$(jq -rs '
            def f: if type == "string" then . elif type == "number" then tostring else "" end;
            def nonul: explode | map(select(. != 0)) | implode;
            if length != 1 or (.[0] | type) != "object" then error("not a vmess object") else .[0] end
            | (.port | if type == "number" then (if . == floor and . >= 1 and . <= 65535 then ["number", (floor | tostring)] else ["bad", tostring] end)
                       elif type == "string" then ["string", .] else ["missing", ""] end) as $port
            | ([.add, .id, .scy, .net, .type, .host, .path, .tls, .sni, .alpn, .fp, .pbk, .sid, .spx]
               | map(f | index("\u0000") != null) | any) as $nul
            | "V_ps=\(.ps | f | nonul | @sh) V_add=\(.add | f | nonul | @sh) V_id=\(.id | f | nonul | @sh)",
              "V_scy=\(.scy | f | nonul | @sh) V_net=\(.net | f | nonul | @sh) V_type=\(.type | f | nonul | @sh)",
              "V_host=\(.host | f | nonul | @sh) V_path=\(.path | f | nonul | @sh) V_tls=\(.tls | f | nonul | @sh)",
              "V_sni=\(.sni | f | nonul | @sh) V_alpn=\(.alpn | f | nonul | @sh) V_fp=\(.fp | f | nonul | @sh)",
              "V_pbk=\(.pbk | f | nonul | @sh) V_sid=\(.sid | f | nonul | @sh) V_spx=\(.spx | f | nonul | @sh)",
              "V_portkind=\($port[0] | @sh) V_port=\($port[1] | nonul | @sh) V_nul=\(if $nul then 1 else 0 end)"
        ' <<< "$text" 2>/dev/null); then
        _sub_skip invalid "vmess link is neither form"
        return 1
    fi
    local V_ps V_add V_id V_scy V_net V_type V_host V_path V_tls V_sni V_alpn V_fp V_pbk V_sid V_spx
    local V_portkind V_port V_nul
    eval "$fields"
    _SUB_RAWNAME=$V_ps
    if [[ $V_nul == 1 ]]; then
        _sub_skip invalid "NUL byte"
        return 1
    fi
    if [[ -z $V_id ]]; then
        _sub_skip invalid "missing id"
        return 1
    fi
    _SUB_HOST=${V_add#\[}
    _SUB_HOST=${_SUB_HOST%\]}
    if [[ -z $_SUB_HOST ]]; then
        _sub_skip invalid "missing address"
        return 1
    fi
    case $V_portkind in
        number) _SUB_PORT=$V_port ;;
        string) _sub_port "$V_port" || return 1 ;;
        bad)
            _sub_skip invalid "bad port $V_port"
            return 1
            ;;
        *)
            _sub_skip invalid "missing port"
            return 1
            ;;
    esac
    _SUB_Q=()
    _SUB_Q[type]=$V_net
    _SUB_Q[security]=none
    if [[ $V_tls == tls || $V_tls == reality ]]; then
        _SUB_Q[security]=$V_tls
    fi
    for k in sni alpn fp pbk sid spx host; do
        vn=V_$k
        _SUB_Q[$k]=${!vn}
    done
    case $V_net in
        grpc) _SUB_Q[serviceName]=$V_path _SUB_Q[mode]=$V_type ;;
        xhttp|splithttp) _SUB_Q[path]=$V_path _SUB_Q[mode]=$V_type ;;
        ''|tcp|raw) _SUB_Q[headerType]=$V_type ;;
        *) _SUB_Q[path]=$V_path ;;
    esac
    _sub_stream none || return 1
    _SUB_V[user]=$V_id
    _SUB_V[enc]=${V_scy:-auto}
    _sub_vmess_record
}

# SIP002 ss://userinfo@host:port[/][?plugin=…]#name, userinfo method:password in
# plain text or base64, and legacy ss://base64(method:password@host:port)#name.
# Base64 may hold a /, so the userinfo is all of the part before ? up to its
# last @, and only the host part ends at a /.
_sub_ss() {
    local rest=$1 body main query="" ui hostport decoded method password
    body=${rest%%#*}
    if [[ $body != "$rest" ]]; then
        _SUB_RAWNAME=${rest#*#}
    fi
    main=${body%%\?*}
    if [[ $main != "$body" ]]; then
        query=${body#*\?}
    fi
    if [[ $main == *@* ]]; then
        if ! _sub_unescape "${main%@*}" 0; then
            _sub_skip invalid "userinfo: bad percent escape"
            return 1
        fi
        ui=$_SUB_U
        if [[ $ui != *:* ]]; then
            if ! decoded=$(_sub_b64 "$ui"); then
                _sub_skip invalid "userinfo is not base64"
                return 1
            fi
            ui=$(_sub_trim "$decoded")
        fi
        hostport=${main##*@}
        hostport=${hostport%%/*}
    else
        if ! decoded=$(_sub_b64 "$main"); then
            _sub_skip invalid "legacy link is not base64"
            return 1
        fi
        decoded=$(_sub_trim "$decoded")
        if [[ $decoded != *@* ]]; then
            _sub_skip invalid "legacy link has no @"
            return 1
        fi
        ui=${decoded%@*}
        hostport=${decoded##*@}
    fi
    # A Shadowsocks 2022 password may hold a colon of its own.
    method=${ui%%:*}
    password=""
    if [[ $ui == *:* ]]; then
        password=${ui#*:}
    fi
    if [[ -z $method || -z $password ]]; then
        _sub_skip invalid "missing method or password"
        return 1
    fi
    _sub_hostport "$hostport" || return 1
    if [[ $_SUB_HASPORT != 1 ]]; then
        _sub_skip invalid "missing port"
        return 1
    fi
    _sub_port "$_SUB_RAWPORT" || return 1
    if ! _sub_query "$query"; then
        _sub_skip invalid "query: bad percent escape"
        return 1
    fi
    if [[ -n ${_SUB_Q[plugin]:-} ]]; then
        _sub_skip unsupported "ss plugin"
        return 1
    fi
    method=${method,,}
    case $method in
        aes-128-gcm|aes-256-gcm|chacha20-poly1305|chacha20-ietf-poly1305|xchacha20-poly1305|\
        xchacha20-ietf-poly1305|2022-blake3-aes-128-gcm|2022-blake3-aes-256-gcm|\
        2022-blake3-chacha20-poly1305|none|plain) ;;
        *)
            _sub_skip unsupported "ss method $method"
            return 1
            ;;
    esac
    _SUB_V[method]=$method
    _SUB_V[password]=$password
    _sub_record '{protocol: "shadowsocks", settings: {servers: [{address: $addr, port: $port,
        method: $method, password: $password}]}}'
}

# hysteria2://auth@host[:port][/]?query#name, hy2:// alike. The port defaults
# to 443. Port hopping is left out: its keys moved between Xray 26.2 and 26.3.
_sub_hy2() {
    _sub_link "$1" || return 1
    _SUB_PORT=443
    if [[ $_SUB_HASPORT == 1 ]]; then
        if [[ $_SUB_RAWPORT == *[,-]* ]]; then
            _sub_skip unsupported "port hopping"
            return 1
        fi
        _sub_port "$_SUB_RAWPORT" || return 1
    fi
    if ! _sub_query "$_SUB_RAWQUERY"; then
        _sub_skip invalid "query: bad percent escape"
        return 1
    fi
    local obfs=${_SUB_Q[obfs]:-}
    case $obfs in
        '') ;;
        salamander)
            if [[ -z ${_SUB_Q[obfs-password]:-} ]]; then
                _sub_skip invalid "salamander needs obfs-password"
                return 1
            fi
            ;;
        *)
            _sub_skip unsupported "hysteria2 obfs $obfs"
            return 1
            ;;
    esac
    if _sub_truthy "${_SUB_Q[insecure]:-}" && [[ -z ${_SUB_Q[pinSHA256]:-} ]]; then
        _sub_skip unsupported "insecure TLS"
        return 1
    fi
    _SUB_V[user]=$_SUB_USER
    _SUB_V[sni]=${_SUB_Q[sni]:-}
    _SUB_V[alpn]=${_SUB_Q[alpn]:-}
    _SUB_V[pin]=${_SUB_Q[pinSHA256]:-}
    _SUB_V[obfs]=$obfs
    _SUB_V[obfspw]=${_SUB_Q[obfs-password]:-}
    _sub_record '{protocol: "hysteria", settings: {version: 2, address: $addr, port: $port},
        streamSettings: ({network: "hysteria", security: "tls",
            hysteriaSettings: {version: 2, auth: $user},
            tlsSettings: {serverName: $sni, alpn: ($alpn | list | if length == 0 then ["h3"] else . end),
                          pinnedPeerCertSha256: $pin}}
          + (if $obfs == "salamander" then {finalmask: {udp: [{type: "salamander", settings: {password: $obfspw}}]}}
             else {} end))}'
}

# _sub_entry <scheme> <rest>: converts one link and keeps its record and raw
# name for the assembly.
_sub_entry() {
    local scheme=${1,,} rest=$2
    _SUB_RAWNAME="" _SUB_DECODE=1 _SUB_REASON="" _SUB_DETAIL="" _SUB_REC="" _SUB_USER=""
    _SUB_V=()
    case $scheme in
        vless) _sub_vless "$rest" || : ;;
        vmess) _sub_vmess "$rest" || : ;;
        trojan) _sub_trojan "$rest" || : ;;
        ss) _sub_ss "$rest" || : ;;
        hysteria2|hy2) _sub_hy2 "$rest" || : ;;
        *)
            if [[ $rest == *#* ]]; then
                _SUB_RAWNAME=${rest#*#}
            fi
            _sub_skip unsupported "$scheme"
            ;;
    esac
    if [[ -n $_SUB_REASON || -z $_SUB_REC ]]; then
        _SUB_REC=$(jq -cn --arg reason "${_SUB_REASON:-invalid}" --arg detail "$_SUB_DETAIL" \
            '{reason: $reason, detail: $detail}')
    fi
    _SUB_RECORDS+=("$_SUB_REC")
    _SUB_RAWNAMES+=("$_SUB_DECODE"$'\t'"$_SUB_RAWNAME")
}

# _sub_links <text>: one link per line; other lines are ignored (spec 6.1).
_sub_links() {
    local line
    while IFS= read -r line || [[ -n $line ]]; do
        line=${line#"${line%%[!$_SUB_SPACE]*}"}
        line=${line%"${line##*[!$_SUB_SPACE]}"}
        [[ $line =~ ^([A-Za-z][A-Za-z0-9+.-]*):// ]] || continue
        _sub_entry "${BASH_REMATCH[1]}" "${line#*://}"
    done <<< "$1"
    if (( ${#_SUB_RECORDS[@]} == 0 )); then
        printf 'unrecognized subscription format\n' >&2
        return 1
    fi
}

# _sub_result: the Result JSON from _SUB_RECORDS and _SUB_RAWNAMES - names,
# the placeholder test (spec 6.6) and the split into servers and skips.
_sub_result() {
    local names
    names=$(printf '%s\n' "${_SUB_RAWNAMES[@]}" | _sub_clean_names) || return 1
    {
        printf '[%s]\n' "${names//$'\n'/,}"
        printf '%s\n' "${_SUB_RECORDS[@]}"
    } | jq -cs '
        def placeholder:
          . == "::" or . == "::1"
          or (split(".") as $p
              | ($p | length) == 4
                and all($p[]; length >= 1 and length <= 3 and (explode | all(.[]; . >= 48 and . <= 57)))
                and all($p[]; tonumber <= 255)
                and (($p[0] | tonumber) == 0 or ($p[0] | tonumber) == 127));
        .[0] as $names | .[1:] as $recs
        | [range(0; $recs | length) as $i
           | $recs[$i] + {name: $names[$i], n: ($i + 1)}
           | if .reason == null and (.address | placeholder) then
               . + {reason: "placeholder", detail: .address} else . end] as $all
        | {total: ($recs | length),
           servers: [$all[] | select(.reason == null)
                     | {name: (if .name == "" then .address else .name end), address, port, outbound}],
           skipped: [$all[] | select(.reason != null)
                     | {name: (if .name == "" then "#\(.n)" else .name end), reason, detail}]}'
}

# subscription_decode: reads a subscription on stdin (spec 6.1) and prints
# the Result JSON; see the header of this file.
subscription_decode() {
    local LC_ALL=C body
    _SUB_RECORDS=() _SUB_RAWNAMES=()
    # bash cannot hold a NUL; the Go side drops them as well.
    body=$(tr -d '\000')
    body=${body#$'\xEF\xBB\xBF'}
    body=$(_sub_trim "$body")
    if [[ -z $body ]]; then
        printf 'empty subscription\n' >&2
        return 1
    fi
    if [[ $body == *'://'* ]]; then
        _sub_links "$body" || return 1
    else
        local text
        if ! text=$(_sub_b64 "$body"); then
            printf 'unrecognized subscription format\n' >&2
            return 1
        fi
        _sub_links "$text" || return 1
    fi
    _sub_result
}
