---
name: promo
description: Post the live Zenitsu Thunder Shop catalog to Telegram groups from SSH host AwsHosting. Use when the user says do, do it, do promo, promo, or /promo. Send only on do/do it/do promo; if they only say promo, wait.
---

# Promo

The live shop, Telethon session, and catalog are on **AwsHosting**. Do not post from this laptop.

Do **not** send when the user only says promo. Wait. Send only when they say **do**, **do it**, or **do promo**.

## Command

```bash
ssh -o BatchMode=yes AwsHosting 'cd /opt/doraemon-shop-bot && scripts/promo/.venv/bin/python scripts/promo/post.py --force'
```

Dry-run only when asked:

```bash
ssh -o BatchMode=yes AwsHosting 'cd /opt/doraemon-shop-bot && scripts/promo/.venv/bin/python scripts/promo/post.py --dry-run'
```

## Behavior already in the script

- Reads `/var/lib/doraemon-shop/store.json`
- Posts only **active, in-stock** products
- Header is `⚡ Zenitsu Thunder Shop`
- Closes with `Check bio for bot and order.`
- Posts only to `ChatgptPlusDeal`

## After it runs

Report whether the group posted or failed. Do not print stock payloads, `.env`, API hash, session files, or 2FA material.
