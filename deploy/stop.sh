#!/usr/bin/env bash
# Останавливает службу scanstore. Сервер дожидается текущих запросов (до 30 секунд), systemd ждёт его до 40.
# Код возврата: 0 - служба остановлена (или уже была остановлена), 1 - остановить не удалось.
set -euo pipefail
. "$(dirname "$0")/common.sh"

if ! systemctl is-active --quiet "$SERVICE"; then
    state="$(systemctl is-active "$SERVICE" 2>/dev/null || true)"
    if [ "$state" = "activating" ]; then
        # служба ждёт перезапуска после сбоя - остановка отменит его
        echo "Служба $SERVICE перезапускается после сбоя, останавливаю..."
    else
        echo "Служба $SERVICE уже остановлена (${state:-inactive})."
        exit 0
    fi
else
    echo "Останавливаю $SERVICE (ожидание текущих запросов до 30 с)..."
fi

ctl stop "$SERVICE"

state="$(systemctl is-active "$SERVICE" 2>/dev/null || true)"
if [ "$state" = "active" ] || [ "$state" = "deactivating" ]; then
    echo "ОШИБКА: служба не остановилась (состояние: $state)." >&2
    exit 1
fi
echo "Готово: служба остановлена (состояние: ${state:-inactive})."
