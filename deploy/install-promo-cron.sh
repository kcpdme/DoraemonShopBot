#!/bin/sh
# Install or refresh the hourly Telethon promo crontab for this checkout.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
PYTHON="$ROOT/scripts/promo/.venv/bin/python"
POST="$ROOT/scripts/promo/post.py"
LOG="$ROOT/data/promo.log"
MARKER="DoraemonShopBot promo"

if [ ! -x "$PYTHON" ]; then
    echo "missing venv interpreter: $PYTHON" >&2
    echo "create it with: python3 -m venv scripts/promo/.venv && scripts/promo/.venv/bin/pip install -r scripts/promo/requirements.txt" >&2
    exit 1
fi
if [ ! -f "$POST" ]; then
    echo "missing poster: $POST" >&2
    exit 1
fi

mkdir -p "$ROOT/data"
touch "$LOG"

LINE="12 * * * * cd $ROOT && $PYTHON $POST >> $LOG 2>&1"

EXISTING=$(crontab -l 2>/dev/null || true)
FILTERED=$(printf '%s\n' "$EXISTING" | grep -v "$MARKER" | grep -v "$POST" || true)
{
    printf '%s\n' "$FILTERED" | sed '/^$/d'
    echo "# $MARKER — every hour"
    echo "$LINE"
} | crontab -

echo "installed hourly cron:"
echo "$LINE"
echo "logs: $LOG"
echo "login once before the first tick: $PYTHON $POST --login"
