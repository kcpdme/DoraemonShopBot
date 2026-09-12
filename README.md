# Doraemon Shop Bot

An open-source Telegram storefront for digital goods, written in Go. Buyers can shop through chat or an optional Telegram Mini App, pay with USDT on BNB Smart Chain or Polygon, and receive inventory privately after on-chain confirmation.

The project has no database or framework dependency. A single process runs the Telegram bot, Mini App API, static web assets, blockchain verification, and a local JSON store.

> [!IMPORTANT]
> Telegram requires Telegram Stars for many digital-goods transactions inside Telegram. This project implements an external USDT flow, but operators are responsible for Telegram policy, consumer-protection, tax, sanctions, and local-law compliance.

## Features

- Responsive Telegram Mini App with live catalog, quantity and network selection, stock reservation, payment details, and order history.
- Full chat-based catalog and three-step checkout for users who prefer bot buttons.
- FIFO inventory with one private delivery payload per stock unit.
- Thirty-minute reservations, duplicate-checkout protection, automatic expiry, and stock release.
- Exact USDT verification: chain ID, token contract, destination, amount, receipt success, transaction age, and three confirmations.
- Durable delivery queue with retry behavior and owner emergency controls.
- Owner-only product wizard, stock management, order review, broadcasts, dashboard, and support replies.
- Optional channel-membership gate and public restock/sale announcements.
- No third-party Go modules, explorer API key, frontend build step, or external database.

## Requirements

- Go 1.22 or newer
- A Telegram bot created with [@BotFather](https://t.me/BotFather)
- An optional public Telegram channel where the bot is an administrator
- BNB Smart Chain and Polygon addresses that can receive USDT
- For the Mini App: a public HTTPS origin reverse-proxied to the bot's local HTTP listener

## Quick start

1. Clone the repository and enter it.

   ```bash
   git clone https://github.com/your-account/doraemon-shop-bot.git
   cd doraemon-shop-bot
   ```

2. Create a local environment file. It is ignored by Git.

   ```bash
   cp .env.example .env
   ```

3. Fill in `.env` with your own bot token, numeric Telegram owner ID, receiving wallets, and official token contracts. Never commit this file.

4. Export the values and start the bot.

   ```bash
   set -a
   source .env
   set +a
   go run ./cmd/bot
   ```

5. Send `/start` to the bot. The configured owner sees the owner panel and can create a product, then add stock.

Each stock line is one deliverable unit—for example, one license key, account bundle, or private download link.

## Configuration

| Variable | Required | Purpose |
| --- | --- | --- |
| `BOT_TOKEN` | Yes | Secret token issued by BotFather. |
| `OWNER_TELEGRAM_ID` | Yes | Numeric Telegram user ID allowed to administer the store. |
| `SALES_CHANNEL` | No | Public `@channel` handle or numeric channel ID. Enables membership gating and announcements. |
| `BEP20_USDT_ADDRESS` | Yes | Receiving wallet for BNB Smart Chain payments. |
| `POLYGON_USDT_ADDRESS` | Yes | Receiving wallet for Polygon payments. |
| `BEP20_USDT_CONTRACT` | Yes | Exact official USDT contract on BNB Smart Chain. |
| `POLYGON_USDT_CONTRACT` | Yes | Exact official USDT contract on Polygon PoS. |
| `BSC_RPC_URL` | No | Custom BSC JSON-RPC URL; public fallbacks are built in. |
| `POLYGON_RPC_URL` | No | Custom Polygon JSON-RPC URL; public fallbacks are built in. |
| `DATA_FILE` | No | JSON store location; defaults to `data/store.json`. |
| `MINI_APP_PUBLIC_URL` | No | Public HTTPS URL for the Mini App. Empty keeps bot-only mode. |
| `MINI_APP_LISTEN_ADDR` | No | Local web listener; defaults to `:8080`. Prefer `127.0.0.1:8080` behind a proxy. |

The application intentionally does not load `.env` itself. Use your shell, container runtime, systemd `EnvironmentFile`, or secret manager to inject configuration.

## Enable the Telegram Mini App

The Mini App is embedded in the Go binary, so it requires no Node.js installation or separate frontend build.

1. Set a local-only listener and your public HTTPS URL:

   ```dotenv
   MINI_APP_LISTEN_ADDR=127.0.0.1:8080
   MINI_APP_PUBLIC_URL=https://shop.example.com
   ```

2. Reverse-proxy `https://shop.example.com` to `http://127.0.0.1:8080`. Your proxy should manage TLS and forward normal HTTP headers. Do not cache `/api/mini-app/*` responses.

3. Restart the bot. On startup it registers **Open shop** as Telegram's menu button, and `/start` includes an **Open Mini App** button.

4. Open the app from Telegram. Opening the public URL in an ordinary browser shows an authentication error by design because it has no signed Telegram launch data.

Mini App requests are authenticated server-side with Telegram's HMAC signature. Signatures older than one hour are rejected. The API accepts the Telegram identity from signed launch data only; a browser-provided user ID is never trusted. Product delivery payloads are never returned by the Mini App API.

## Buyer flow

1. Browse a product in the Mini App or bot chat.
2. Choose a quantity and either BEP-20 or Polygon.
3. The bot atomically reserves FIFO stock for 30 minutes.
4. Send the exact USDT amount to the shown wallet on the selected network.
5. In the bot chat, tap **I've paid · Submit TxID** and paste the transaction hash or explorer URL.
6. After three confirmations, the bot sends delivery instructions and each private stock payload to that buyer.

Public announcements contain neither buyer identity nor private inventory. A transaction hash cannot fulfill two orders on the same network.

## Owner commands

Most administration is available from `/start` → **Owner panel**.

```text
/admin                         Open the owner panel
/confirm ORDER_ID              Deliver after a manual payment check
/reject ORDER_ID reason        Reject submitted payment and release stock
/reply CHAT_ID message         Reply to a support request
/announce message              Broadcast to users and the sales channel
```

Manual confirmation bypasses blockchain validation. Use it only after independently confirming payment.

## Data and secret safety

- `.env`, private key formats, runtime data, databases, logs, and local binaries are excluded in `.gitignore`.
- `.env.example` contains names and placeholders only. Keep it safe to publish.
- `data/store.json` contains inventory payloads, buyer IDs, orders, and delivery state. Store it on an encrypted/protected volume, mode `0600`, and back it up securely.
- Never place seed phrases, wallet private keys, RPC credentials, or real stock in source files. This bot needs receiving addresses only; it never needs wallet signing keys.
- The file store is single-process. Do not run multiple replicas against the same JSON file. Move persistence and reservation locking to a transactional database before scaling horizontally.
- If a secret has ever been committed, adding it to `.gitignore` is insufficient. Revoke/rotate it and remove it from Git history before publishing.

Before making a fork public, run a secret scanner against the full Git history and inspect `git ls-files` yourself.

## Development

Run the test suite and static analysis:

```bash
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
```

Build a production Linux binary:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o doraemon-bot ./cmd/bot
```

The Mini App frontend lives in `cmd/bot/miniapp/` and is embedded by `cmd/bot/miniapp.go`. API routes are:

- `GET /api/mini-app/catalog`
- `GET /api/mini-app/orders`
- `POST /api/mini-app/orders`
- `GET /healthz` (unauthenticated service health check)

All API routes require a fresh `Authorization: tma <Telegram initData>` header.

## Production deployment

An example hardened systemd unit is included at `deploy/doraemon-shop.service`. It expects:

- binary: `/opt/doraemon-shop-bot/doraemon-bot`
- environment: `/etc/doraemon-shop.env`
- writable data: `/var/lib/doraemon-shop/store.json`

`deploy/release.py` performs preflight checks, backs up the binary/config/data, atomically replaces the binary, restarts the service, and restores the prior release if startup fails. Review and adapt both deployment files for your own Linux account, paths, TLS proxy, and backup policy before use.

## Payment limitations

- Public RPC endpoints can be rate-limited or unavailable. Configure trusted providers for production.
- A shared receiving wallet cannot prove which Telegram user initiated a public transfer. Someone who learns an unclaimed matching TxID could submit it first. Per-order deposit addresses or wallet-ownership proofs are needed to eliminate this limitation.
- Telegram delivery is at-least-once. A crash after Telegram accepts a message but before the delivery cursor is saved can repeat that message.
- Crypto payments can be irreversible. Provide clear support and refund policies.

## Contributing

Issues and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) before submitting changes and [SECURITY.md](SECURITY.md) before reporting a vulnerability. Please keep changes focused, add tests for behavior changes, run `go test -race ./...` and `go vet ./...`, and never include real credentials or inventory in fixtures.

## License

Released under the [MIT License](LICENSE).

This community project is not affiliated with Telegram, Tether, BNB Chain, Polygon, or the owners of the Doraemon trademark.
