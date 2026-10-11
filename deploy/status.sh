#!/usr/bin/env bash
# Показывает состояние службы scanstore: systemd, ответ /health и последние строки журнала.
#   ./status.sh               15 последних строк журнала
#   LOG_LINES=50 ./status.sh  больше строк
# Код возврата как у systemctl status: 0 - работает и API отвечает, 1 - служба активна, но API не отвечает,
# 3 - служба не запущена.
set -uo pipefail
. "$(dirname "$0")/common.sh"

if ! systemctl cat "$SERVICE" >/dev/null 2>&1; then
    echo "Служба $SERVICE не установлена (нет unit-файла в systemd)." >&2
    exit 4
fi

# systemctl show выводит строки Ключ=Значение
prop() { systemctl show "$SERVICE" -p "$1" --value 2>/dev/null; }

active="$(prop ActiveState)"
sub="$(prop SubState)"
echo "Служба:       $SERVICE"
echo "Состояние:    ${active:-?} (${sub:-?})"
echo "Автозапуск:   $(systemctl is-enabled "$SERVICE" 2>/dev/null || echo неизвестно)"

if [ "$active" = "active" ]; then
    echo "Запущена:     $(prop ActiveEnterTimestamp)"
    echo "PID:          $(prop MainPID)"
    mem="$(prop MemoryCurrent)"
    case "$mem" in
        "" | "[not set]" | 18446744073709551615) ;;
        *) echo "Память:       $((mem / 1024 / 1024)) МБ" ;;
    esac
fi
echo "Перезапусков: $(prop NRestarts)"
echo "Адрес API:    $(base_url)"

code=3
if [ "$active" = "active" ]; then
    if answer="$(health)"; then
        echo "/health:      $answer"
        code=0
    else
        echo "/health:      НЕ ОТВЕЧАЕТ"
        code=1
    fi
else
    echo "/health:      служба не запущена"
fi

echo "--- последние строки журнала (${LOG_LINES:-15}):"
recent_log "${LOG_LINES:-15}"
exit "$code"
