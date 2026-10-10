# Reseller API

Customers manage a personal API key through the bot and use it to read current
prices, spend their existing USDT wallet balance, and retrieve their API orders
and purchased delivery items. Prices match the storefront; there is no separate
reseller discount. No supplier API is involved.

This follows MegaMarket's Telegram-linked key lifecycle and adds wallet-funded
orders. It is not wire-compatible with MegaMarket's read-only pricing API.

## Enable the HTTP service

The API shares the existing Mini App listener. Set `MINI_APP_PUBLIC_URL` to the
public HTTPS shop URL and `MINI_APP_LISTEN_ADDR` to the local proxy target, as
described in the main README, then restart the updated bot. No new environment
variables or external services are needed. With `MINI_APP_PUBLIC_URL` empty, the
bot continues to run without an HTTP listener.

For `MINI_APP_PUBLIC_URL=https://shop.example.com`, the API base URL is
`https://shop.example.com/api/v1/reseller`. If the Mini App is hosted under
`https://shop.example.com/shop/`, use
`https://shop.example.com/shop/api/v1/reseller`. Forward these paths through the
HTTPS proxy without caching responses or logging Authorization headers or
response bodies. The bot's `/apikey help` shows the configured base URL.

The JSON store remains single-process. Key hashes and API order snapshots are
persisted with the existing store; old store files load without migration. Keep
backups private because order snapshots include the purchased inventory payloads.

## Customer setup

In a private conversation with the bot, choose **Reseller API** or use:

- `/apikey` — key status and controls.
- `/apikey generate` — create a key, shown once.
- `/apikey rotate` — confirm replacement; the old key stops working.
- `/apikey revoke` — confirm disabling the key.
- `/apikey help` — base URL and request examples.

Each Telegram customer has one active key. Only a SHA-256 digest is saved; the
original key cannot be recovered. Duplicate create requests do not replace an
active key, and old rotation/revocation confirmation buttons cannot affect a new
key. A failed Telegram key delivery leaves the current key active.

A key can spend its customer's wallet funds and retrieve their private API
purchases. Keep it on your backend and send it only over HTTPS. To fund orders,
use `/wallet` in the bot; existing on-chain verification credits the wallet.
Joining the updates channel is optional by default. Only when
`REQUIRE_CHANNEL_MEMBERSHIP=true` is explicitly configured must customers be
members to generate/rotate keys or place API orders. They can still read existing
API purchases and revoke keys after leaving the channel.

## Authentication and prices

All requests require `Authorization: Bearer YOUR_KEY`. Query-string keys and
Telegram Mini App launch data are not accepted as reseller credentials.

```bash
export SHOP_API='https://shop.example.com/api/v1/reseller'
# Set SHOP_API_KEY privately to the key received in the bot.
curl "$SHOP_API/prices" -H "Authorization: Bearer $SHOP_API_KEY"
curl "$SHOP_API/prices/KEY" -H "Authorization: Bearer $SHOP_API_KEY"
curl "$SHOP_API/balance" -H "Authorization: Bearer $SHOP_API_KEY"
```

`GET /prices` returns active, validly priced products, including sold-out ones:

```json
{
  "products": [{
    "sku": "KEY",
    "name": "Access key",
    "description": "One access key",
    "priceUsdt": "4.50",
    "currency": "USDT",
    "stock": 2
  }]
}
```

`GET /prices/{sku}` returns a single product under `product`. Use the exact SKU
returned by the catalog. Stock excludes sold and reserved units. Delivery
instructions and inventory payloads are never returned by price endpoints.

`GET /balance` returns `{"balanceUsdt":"20.00","currency":"USDT"}`.
Monetary response values are decimal strings with two places; wallet debits are
calculated in integer cents.

## Place an order

```bash
curl "$SHOP_API/orders" \
  -H "Authorization: Bearer $SHOP_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: my-store-order-123' \
  --data '{"sku":"KEY","quantity":2}'
```

Quantity must be an integer from 1 to 50. `Idempotency-Key` is required and must
contain 1–128 ASCII letters, digits, hyphens, underscores, dots, or colons. Use a
new key for each intended purchase, and reuse the same key and body whenever a
request times out or needs a retry. Never generate a new key merely because a
response was lost.

The server checks the current price, wallet balance, and FIFO stock availability.
The wallet debit, ledger entry, stock sale, delivery snapshot, and idempotency key
are saved atomically. Insufficient balance or stock does not reserve anything or
charge the customer. Failed saves roll back the in-memory change as well.

A successful purchase returns HTTP 201:

```json
{
  "order": {
    "id": "ord_example",
    "sku": "KEY",
    "name": "Access key",
    "quantity": 2,
    "amountUsdt": "9.00",
    "currency": "USDT",
    "status": "delivered",
    "createdAt": "2026-10-02T10:00:00Z",
    "paidAt": "2026-10-02T10:00:00Z",
    "deliveryInstruction": "Redeem privately",
    "items": ["purchased-item-one", "purchased-item-two"]
  }
}
```

API orders are delivered through JSON, with `delivered` meaning the purchased
items are saved and retrievable. They do not depend on Telegram message delivery.
They also appear in the bot's order history and wallet ledger. The API does not
send separate Telegram delivery messages or sale announcements.

Repeating a successful request returns HTTP 200, the same order, and
`Idempotency-Replayed: true`, without charging again. Reusing a key with a different
SKU or quantity returns HTTP 409 with `idempotency_conflict`. Successful keys stay
attached to the customer's orders across restarts and API-key rotation/revocation.
Delivery remains retrievable even if the product or sold stock is later deleted.
Failed purchases do not consume their idempotency keys; retrying them uses current
price and availability.

## Order details and history

```bash
curl "$SHOP_API/orders/ord_example" -H "Authorization: Bearer $SHOP_API_KEY"
curl "$SHOP_API/orders?limit=50&offset=0" -H "Authorization: Bearer $SHOP_API_KEY"
```

`GET /orders/{id}` returns the same detailed `order` object, including private
purchased items. Another customer's order ID returns HTTP 404.

`GET /orders` returns newest-first summaries under `orders`, plus `total`, `limit`,
and `offset`. Summaries omit delivery instructions and payloads. `limit` defaults
to 50 and must be 1–100; `offset` defaults to 0 and must be non-negative. These
endpoints expose API-originated orders only. Chat/Mini App purchases continue to
use the bot's existing history and delivery flow.

## Errors and limits

Errors use `{"code":"insufficient_balance","error":"Add USDT funds ..."}`.

- HTTP 400: invalid JSON, SKU syntax, quantity, pagination, or idempotency key.
- HTTP 401: missing, invalid, rotated, or revoked API key.
- HTTP 403: required channel membership is missing.
- HTTP 404: unknown endpoint/product or an order unavailable to this customer.
- HTTP 405: unsupported method; see the `Allow` header.
- HTTP 409: `insufficient_balance`, `insufficient_stock`, `invalid_price`, or
  `idempotency_conflict`.
- HTTP 429: more than 60 authenticated requests per minute per customer;
  wait for `Retry-After` seconds. Rotation does not reset this limit.
- HTTP 500: order persistence failed; retry with the same idempotency key.
- HTTP 503: channel membership could not be verified; retry later.

All reseller responses include `Cache-Control: no-store`. The rate limiter is
local to the process and resets on restart, consistent with the file store's
single-process deployment model.
