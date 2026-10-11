#!/usr/bin/env bash
# Общие функции для start.sh, stop.sh и status.sh (подключается через source, сам не запускается).
#
# Переменные окружения:
#   SCANSTORE_SERVICE  имя службы systemd              (по умолчанию scanstore)
#   SCANSTORE_CONFIG   путь к config.ini                (по умолчанию /etc/scanstore/config.ini)
#   SCANSTORE_URL      адрес API для проверки /health  (по умолчанию из [server] host и port в config.ini)

SERVICE="${SCANSTORE_SERVICE:-scanstore}"
CONFIG="${SCANSTORE_CONFIG:-/etc/scanstore/config.ini}"

# Запуск и останов службы требуют root; под обычным пользователем команда идёт через sudo.
ctl() {
    if [ "$(id -u)" -eq 0 ]; then
        systemctl "$@"
    else
        sudo systemctl "$@"
    fi
}

# Значение ключа из секции ini-файла (комментарии - только отдельной строкой, как у самого сервера).
ini_value() { # секция ключ
    [ -r "$CONFIG" ] || return 1
    awk -v want_section="$1" -v want_key="$2" '
        /^[ \t]*[;#]/ { next }
        /^[ \t]*\[/ {
            section = tolower($0); gsub(/[][ \t\r]/, "", section); next
        }
        section == want_section && index($0, "=") {
            key = substr($0, 1, index($0, "=") - 1); gsub(/[ \t]/, "", key)
            if (tolower(key) == want_key) {
                value = substr($0, index($0, "=") + 1)
                sub(/^[ \t]+/, "", value); sub(/[ \t\r]+$/, "", value)
                found = value
            }
        }
        END { if (found != "") print found; else exit 1 }
    ' "$CONFIG"
}

# Адрес API без завершающего слэша.
base_url() {
    if [ -n "${SCANSTORE_URL:-}" ]; then
        echo "${SCANSTORE_URL%/}"
        return
    fi
    local host port
    host="$(ini_value server host || true)"
    port="$(ini_value server port || true)"
    case "$host" in
        "" | 0.0.0.0 | "::") host="127.0.0.1" ;;
    esac
    case "$host" in
        *:*) host="[$host]" ;; # IPv6
    esac
    echo "http://${host}:${port:-8000}"
}

# Ответ /health (одной строкой) или ненулевой код, если API не отвечает.
health() {
    curl -fsS --max-time 3 "$(base_url)/health" 2>/dev/null
}

# Ждёт, пока API ответит на /health: wait_health <секунд>. Прекращает ждать, если служба упала.
wait_health() {
    local deadline=$((SECONDS + $1))
    while [ "$SECONDS" -lt "$deadline" ]; do
        if health >/dev/null; then
            return 0
        fi
        if [ "$(systemctl is-failed "$SERVICE" 2>/dev/null || true)" = "failed" ]; then
            return 1
        fi
        sleep 1
    done
    return 1
}

# Последние строки журнала службы: recent_log [число строк].
recent_log() {
    journalctl -u "$SERVICE" -n "${1:-15}" --no-pager 2>&1 || true
}
