#!/usr/bin/env python3
"""Post a catalog promo to Telegram groups with Telethon.

One run sends one message per group, then exits. Point cron at this file
once an hour after you have logged in a user session.

First-time login (interactive; saves the session file):

    python3 scripts/promo/post.py --login

Hourly cron (after login):

    12 * * * * cd /path/to/DoraemonShopBot && scripts/promo/.venv/bin/python scripts/promo/post.py >> data/promo.log 2>&1

Required env (also loaded from repo-root .env if present):

    TELEGRAM_API_ID, TELEGRAM_API_HASH
"""

from __future__ import annotations

import argparse
import fcntl
import json
import os
import sys
import time
from datetime import datetime, timezone
from html import escape
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_GROUPS = ("ChatgptPlusDeal",)
HEADER = "⚡ <b>Zenitsu Thunder Shop</b>"
CTA = "Check bio for bot and order."
TELEGRAM_TEXT_LIMIT = 4096


def load_env_file(path: Path) -> None:
    if not path.is_file():
        return
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        if not key or key in os.environ:
            continue
        os.environ[key] = value.strip().strip("'").strip('"')


def env_path(name: str, default: str) -> Path:
    raw = os.environ.get(name, default).strip() or default
    path = Path(raw).expanduser()
    if not path.is_absolute():
        path = REPO_ROOT / path
    return path


def parse_groups(raw: str | None) -> list[str]:
    if raw is None:
        values = list(DEFAULT_GROUPS)
    else:
        values = [part.strip() for part in raw.replace("\n", ",").split(",")]
    groups: list[str] = []
    seen: set[str] = set()
    for value in values:
        name = parse_group(value)
        if not name or name.lower() in seen:
            continue
        seen.add(name.lower())
        groups.append(name)
    return groups


def parse_group(value: str) -> str:
    name = value.strip()
    if not name:
        return ""
    name = name.split("?")[0].split("#")[0]
    for prefix in ("https://t.me/", "http://t.me/", "https://telegram.me/", "http://telegram.me/", "t.me/", "telegram.me/"):
        if name.lower().startswith(prefix):
            name = name[len(prefix) :]
            break
    name = name.strip("/")
    if name.startswith("@"):
        name = name[1:]
    if "/" in name:
        name = name.split("/", 1)[0]
    return name.strip()


def stock_count(stock: dict, sku: str) -> int:
    n = 0
    for item in stock.values():
        if not isinstance(item, dict):
            continue
        if item.get("SKU") != sku:
            continue
        if item.get("Sold") or item.get("OrderID"):
            continue
        n += 1
    return n


def load_products(store_path: Path) -> list[dict]:
    if not store_path.is_file():
        raise FileNotFoundError(f"store not found: {store_path}")
    data = json.loads(store_path.read_text(encoding="utf-8"))
    products = data.get("Products") or {}
    stock = data.get("Stock") or {}
    rows: list[dict] = []
    for sku, product in products.items():
        if not isinstance(product, dict) or not product.get("Active"):
            continue
        available = stock_count(stock, product.get("SKU") or sku)
        if available <= 0:
            continue
        created = product.get("CreatedAt") or ""
        rows.append(
            {
                "sku": sku,
                "name": str(product.get("Name") or sku).strip() or sku,
                "price": float(product.get("PriceUSDT") or 0),
                "stock": available,
                "created": created,
            }
        )
    rows.sort(key=lambda row: row["created"], reverse=True)
    return rows


def format_promo(products: list[dict]) -> str:
    if not products:
        return ""
    lines = [HEADER, ""]
    for product in products:
        name = escape(product["name"])
        lines.append(f"• <b>{name}</b> — ${product['price']:.2f} USDT")
    lines.extend(["", escape(CTA)])
    text = "\n".join(lines)
    if len(text) <= TELEGRAM_TEXT_LIMIT:
        return text
    # Keep the CTA and drop the oldest / last listed products until it fits.
    keep = list(products)
    while keep:
        keep.pop()
        text = format_promo(keep)
        if len(text) <= TELEGRAM_TEXT_LIMIT:
            return text
    return escape(CTA)


def acquire_lock(path: Path):
    path.parent.mkdir(parents=True, exist_ok=True)
    handle = open(path, "a+", encoding="utf-8")
    try:
        fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        handle.close()
        raise SystemExit("another promo run is already in progress")
    return handle


def require_api_credentials() -> tuple[int, str]:
    api_id_raw = os.environ.get("TELEGRAM_API_ID", "").strip()
    api_hash = os.environ.get("TELEGRAM_API_HASH", "").strip()
    if not api_id_raw or not api_hash:
        raise SystemExit("set TELEGRAM_API_ID and TELEGRAM_API_HASH (from https://my.telegram.org)")
    try:
        api_id = int(api_id_raw)
    except ValueError as exc:
        raise SystemExit("TELEGRAM_API_ID must be an integer") from exc
    return api_id, api_hash


def new_client():
    from telethon import TelegramClient
    from telethon.sessions import StringSession

    api_id, api_hash = require_api_credentials()
    string_session = os.environ.get("TELEGRAM_SESSION_STRING", "").strip()
    if string_session:
        return TelegramClient(StringSession(string_session), api_id, api_hash)
    session_path = env_path("TELEGRAM_SESSION", "data/promo.session")
    session_path.parent.mkdir(parents=True, exist_ok=True)
    return TelegramClient(str(session_path), api_id, api_hash)


def login_state_path() -> Path:
    return env_path("TELEGRAM_SESSION", "data/promo.session").with_name("promo.login.json")


def read_login_state() -> dict:
    path = login_state_path()
    if not path.is_file():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return {}
    return data if isinstance(data, dict) else {}


def write_login_state(state: dict) -> None:
    path = login_state_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(state, indent=2) + "\n", encoding="utf-8")
    os.chmod(path, 0o600)


def clear_login_state() -> None:
    path = login_state_path()
    if path.is_file():
        path.unlink()


def logged_in_as(me) -> str:
    return str(getattr(me, "username", None) or getattr(me, "id", "?"))


async def login(phone: str | None = None, code: str | None = None, password: str | None = None) -> int:
    from telethon.errors import PhoneCodeExpiredError, PhoneCodeInvalidError, SessionPasswordNeededError

    phone = (phone or os.environ.get("TELEGRAM_PHONE") or "").strip()
    code = (code or "").strip()
    password = password or ""
    state = read_login_state()
    if not phone:
        phone = str(state.get("phone") or "").strip()

    client = new_client()
    await client.connect()
    try:
        if await client.is_user_authorized():
            me = await client.get_me()
            clear_login_state()
            print(f"already logged in as {logged_in_as(me)}")
            return 0

        if not phone:
            print("pass the account phone with country code, e.g. --phone +2348012345678", file=sys.stderr)
            return 1

        if not code and not password:
            sent = await client.send_code_request(phone)
            write_login_state({"phone": phone, "phone_code_hash": sent.phone_code_hash})
            print("Telegram sent a login code to that account. Reply with the code.")
            return 0

        try:
            if code:
                try:
                    await client.sign_in(phone=phone, code=code, phone_code_hash=state.get("phone_code_hash"))
                except SessionPasswordNeededError:
                    if not password:
                        write_login_state({"phone": phone, "phone_code_hash": state.get("phone_code_hash")})
                        print("this account has 2FA cloud password. rerun with --login --password 'your password'", file=sys.stderr)
                        return 1
                    await client.sign_in(password=password)
            elif password:
                await client.sign_in(password=password)
        except PhoneCodeExpiredError:
            clear_login_state()
            print("that code expired. rerun with --login --phone and a new code", file=sys.stderr)
            return 1
        except PhoneCodeInvalidError:
            print("that code was invalid. check it and rerun with --login --code", file=sys.stderr)
            return 1

        me = await client.get_me()
        clear_login_state()
        print(f"logged in as {logged_in_as(me)}")
        return 0
    finally:
        await client.disconnect()


async def post(dry_run: bool) -> int:
    products = load_products(env_path("DATA_FILE", "data/store.json"))
    groups = parse_groups(os.environ.get("PROMO_GROUPS"))
    text = format_promo(products)
    delay = float(os.environ.get("PROMO_DELAY_SECONDS", "8") or "8")
    if delay < 0:
        delay = 0

    if not groups:
        print("no promo groups configured", file=sys.stderr)
        return 1
    if not text:
        print("no active products; skipping post")
        return 0

    print(f"{datetime.now(timezone.utc).isoformat()} products={len(products)} groups={','.join(groups)}")
    if dry_run:
        print(text)
        print("dry-run: not sending")
        return 0

    from telethon.errors import FloodWaitError, RPCError

    client = new_client()
    failures = 0
    async with client:
        if not await client.is_user_authorized():
            print("session is not logged in; run: python3 scripts/promo/post.py --login", file=sys.stderr)
            return 1
        for index, group in enumerate(groups):
            if index and delay:
                time.sleep(delay)
            try:
                await client.send_message(group, text, parse_mode="html", link_preview=False)
                print(f"posted to {group}")
            except FloodWaitError as err:
                print(f"flood wait {err.seconds}s on {group}; sleeping", file=sys.stderr)
                time.sleep(err.seconds + 1)
                try:
                    await client.send_message(group, text, parse_mode="html", link_preview=False)
                    print(f"posted to {group} after flood wait")
                except Exception as retry_err:
                    print(f"failed {group} after flood wait: {retry_err}", file=sys.stderr)
                    failures += 1
            except (RPCError, ValueError) as err:
                print(f"failed {group}: {err}", file=sys.stderr)
                failures += 1
    return 1 if failures else 0


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Post shop promo to Telegram groups via Telethon")
    parser.add_argument("--login", action="store_true", help="Telethon login, then exit")
    parser.add_argument("--phone", help="phone with country code, used with --login")
    parser.add_argument("--code", help="Telegram login code, used with --login")
    parser.add_argument("--password", help="2FA cloud password, used with --login")
    parser.add_argument("--dry-run", action="store_true", help="print the promo and targets without sending")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    load_env_file(REPO_ROOT / ".env")
    args = parse_args(argv)
    if args.login:
        import asyncio

        return asyncio.run(login(args.phone, args.code, args.password))

    lock = acquire_lock(env_path("PROMO_LOCK", "data/promo.lock"))
    try:
        import asyncio

        return asyncio.run(post(args.dry_run))
    except FileNotFoundError as err:
        print(str(err), file=sys.stderr)
        return 1
    finally:
        lock.close()


if __name__ == "__main__":
    sys.exit(main())
