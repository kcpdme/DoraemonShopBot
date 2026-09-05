# Doraemon Shop Bot

A Telegram-only digital-goods store with no frontend. It keeps a per-product delivery instruction, FIFO stock units, two USDT payment rails, owner-only administration, buyer delivery, and public channel activity messages.

## What it does

- Customer catalog, product details, stock visibility, quantity presets/custom quantities, and a three-step BEP-20/Polygon checkout with editable payment cards.
- Reserves FIFO stock for 30 minutes. Repeated checkout taps resume the same order. Unpaid cancellation/expiry releases stock; submitted payments keep their reservation while being checked.
- Customer submits a transaction hash. The bot verifies the exact USDT contract, receiving wallet, amount, successful receipt, and three block confirmations before delivering the reserved stock automatically. The owner retains `/confirm ORDER_ID` as an emergency fallback.
- On confirmation the buyer receives the purchased item list, delivery instruction, and their private stock payload. The public channel sees only the product purchase, never credentials or buyer identity.
- Every `/stock` addition sends a restock announcement to every user who started the bot and to the configured channel.
- The owner-only in-chat panel creates products through a guided wizard, accepts single or bulk stock (one private payload per line), shows products/orders/dashboard, hides products, broadcasts announcements, and keeps emergency delivery controls.
- Customers have paginated order history, payment recovery controls, and support messaging. The owner can use Telegram's native Reply action on a support request; `/reply CUSTOMER_CHAT_ID message` remains an optional fallback.
- Channel membership is required for shopping. Support and existing-order payment recovery remain accessible even if a customer leaves the channel.

## Setup

1. Create a bot with [@BotFather](https://t.me/BotFather). Create a public channel, add the bot as an administrator, and set its `@channel` handle in `SALES_CHANNEL`.
2. Copy `.env.example` into your deployment environment and populate every required value. `OWNER_TELEGRAM_ID` is numeric, not an `@username`.
3. Run it with Go 1.22+: `go run ./cmd/bot`.

The bot uses long polling, so no domain, webhook, or frontend is required. Its local data file contains delivery payloads; protect the host volume and back it up. For multi-instance production deployment, replace the JSON store with Postgres (the current file store is intentionally single-process).

## Owner panel

```
/start → Owner panel
/confirm ORDER_ID
/reject ORDER_ID reason
/reply CUSTOMER_CHAT_ID your message
```

Each stock line is one sellable unit. For example, a licence key, login bundle, or download link can be the entire private payload. Product delivery instructions are sent on every confirmed delivery along with the purchased item list.

## Automatic payment operation

No Etherscan account or API key is required. Customers tap **I've paid · Submit TxID** and paste a hash or explorer link; `/paid ORDER_ID TX_HASH` also works. The bot checks the selected chain through public JSON-RPC providers. It only auto-delivers after checking the chain ID, transaction age, exact USDT contract, receiving wallet, exact amount, successful receipt and three confirmations. A transaction cannot fulfill multiple orders on the same chain.

Only potentially recoverable states (a transaction not yet visible, insufficient confirmations, or provider outages) retry every 30 seconds. A permanently invalid proof—old transaction, failed receipt, wrong token/wallet or amount—stops automatic verification and offers a replacement TxID or support. Rejected proof is distinct from a rejected order.

Confirmed payments enter a durable delivery queue. Telegram failures retry delivery without rechecking payment; the purchased list, instructions and private stock are delivered in saved parts. A crash after Telegram accepts a message but before its cursor is saved may repeat that part (at-least-once delivery). Public sales posts never contain credentials or customer identities. Channel notification failures currently require owner follow-up.

The owner can inspect pending payments and use `/confirm ORDER_ID` or `/reject ORDER_ID reason`. Manual confirmation deliberately bypasses blockchain checks and must only be used after independently checking payment. Public TxIDs do not prove payer identity: this shared-wallet design cannot prevent someone claiming another person's still-unclaimed matching transfer. Per-order deposit addresses or wallet-ownership verification would be needed to remove that limitation.

## Deploy an update to the deployment server

Run `GOTOOLCHAIN=local go test -race ./...` and `GOTOOLCHAIN=local go vet ./...`, then build with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/doraemon-bot-release ./cmd/bot`. Copy the binary and `deploy/release.py` into a fresh temporary directory on the server. Run the script there with sudo, passing the staged binary path. It checks Telegram/channel access and both chain/token configurations, stops the service, backs up the binary/config/data under `/var/backups/doraemon-shop`, atomically replaces the binary, and restarts `doraemon-shop`. It never overwrites live inventory with local test data. On startup failure it restores the previous binary and configuration, retaining current live data.

## Platform compliance

Telegram currently says that digital goods sold inside Telegram apps must use Telegram Stars; it warns that third-party payment providers for in-app digital goods can lead to enforcement. This project implements the requested external BEP-20/Polygon flow, but you should obtain legal/platform guidance before operating it in Telegram mobile clients. See Telegram's [digital-goods payment policy](https://core.telegram.org/bots/payments-stars) and [developer terms](https://telegram.org/tos/bot-developers).
