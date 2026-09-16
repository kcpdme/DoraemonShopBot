#!/bin/sh
# Install or refresh the Telethon promo crontab for this checkout.
# Cron ticks every minute; post.py waits a random 3-5 minutes between sends.
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
# Avoid an immediate send on the first cron tick.
SEEDED=$(cd "$ROOT/scripts/promo" && "$PYTHON" -c "from post import mark_posted; print(mark_posted())")

LINE="* * * * * cd $ROOT && $PYTHON $POST >> $LOG 2>&1"

EXISTING=$(crontab -l 2>/dev/null || true)
FILTERED=$(printf '%s\n' "$EXISTING" | grep -v "$MARKER" | grep -v "$POST" || true)
{
    printf '%s\n' "$FILTERED" | sed '/^$/d'
    echo "# $MARKER — every 3-5 minutes (random interval inside the script)"
    echo "$LINE"
} | crontab -

echo "installed promo cron:"
echo "$LINE"
echo "logs: $LOG"
echo "next automatic promo in ${SEEDED}s"
echo "login once before the first tick: $PYTHON $POST --login"
echo "manual send: $PYTHON $POST --force"
