# Doraemon Shop Bot

A Telegram-only digital-goods store with no frontend. It keeps a per-product delivery instruction, FIFO stock units, two USDT payment rails, owner-only administration, buyer delivery, and public channel activity messages.

## What it does

- Customer catalog, product details, stock visibility, and BEP-20/Polygon checkout.
- Reserves a unique stock unit as soon as an order starts; it is released if the owner rejects the payment proof.
- Customer submits a transaction hash. The bot verifies the exact USDT contract, receiving wallet, amount, successful receipt, and three block confirmations before delivering the reserved stock automatically. The owner retains `/confirm ORDER_ID` as an emergency fallback.
- On confirmation the buyer receives the purchased item list, delivery instruction, and their private stock payload. The public channel sees only the product purchase, never credentials or buyer identity.
- Every `/stock` addition sends a restock announcement to every user who started the bot and to the configured channel.
- The owner can create products, add stock, confirm/reject payments, and broadcast custom messages to started users and the channel.

## Setup

1. Create a bot with [@BotFather](https://t.me/BotFather). Create a public channel, add the bot as an administrator, and set its `@channel` handle in `SALES_CHANNEL`.
2. Copy `.env.example` into your deployment environment and populate every required value. `OWNER_TELEGRAM_ID` is numeric, not an `@username`.
3. Run it with Go 1.22+: `go run ./cmd/bot`.

The bot uses long polling, so no domain, webhook, or frontend is required. Its local data file contains delivery payloads; protect the host volume and back it up. For multi-instance production deployment, replace the JSON store with Postgres (the current file store is intentionally single-process).

## Owner commands

```
/admin
/addproduct SKU | Name | price_usdt | description | delivery instruction
/stock SKU | one private delivery payload
/confirm ORDER_ID
/reject ORDER_ID reason
/announce your message
```

Each `/stock` entry is one sellable unit. For example, a licence key, login bundle, or download link can be the entire private payload. Product delivery instructions are sent on every confirmed delivery along with the purchased item list.

## Automatic payment operation

Create one Etherscan API V2 key and configure the exact official USDT contract address for each accepted network. After `/paid ORDER_ID TX_HASH`, the bot checks the selected chain every 30 seconds. It only auto-delivers when the hash has an exact USDT transfer to the configured wallet, for the exact order amount, from the configured token contract, with a successful receipt and at least three confirmations. A submitted hash can only be used once.

If Etherscan is unavailable or a transaction is ambiguous, the order stays pending; it is never auto-rejected. The owner can inspect it and use `/confirm ORDER_ID` or `/reject ORDER_ID reason`.

## Platform compliance

Telegram currently says that digital goods sold inside Telegram apps must use Telegram Stars; it warns that third-party payment providers for in-app digital goods can lead to enforcement. This project implements the requested external BEP-20/Polygon flow, but you should obtain legal/platform guidance before operating it in Telegram mobile clients. See Telegram's [digital-goods payment policy](https://core.telegram.org/bots/payments-stars) and [developer terms](https://telegram.org/tos/bot-developers).
