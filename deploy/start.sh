#!/usr/bin/env bash
# Запускает службу scanstore и ждёт, пока API ответит на /health.
#   ./start.sh            ждёт до 30 секунд
#   WAIT=60 ./start.sh    ждёт до 60 секунд
# Код возврата: 0 - служба запущена и API отвечает, 1 - не запустилась или API не ответил.
set -euo pipefail
. "$(dirname "$0")/common.sh"

if systemctl is-active --quiet "$SERVICE"; then
    echo "Служба $SERVICE уже запущена."
else
    echo "Запускаю $SERVICE..."
    ctl start "$SERVICE"
fi

echo "Жду ответа $(base_url)/health (до ${WAIT:-30} с)..."
if wait_health "${WAIT:-30}"; then
    echo "Готово: служба работает, /health -> $(health)"
    exit 0
fi

echo "ОШИБКА: API не ответил. Состояние службы: $(systemctl is-active "$SERVICE" 2>/dev/null || true)" >&2
echo "--- последние строки журнала:" >&2
recent_log 20 >&2
exit 1
