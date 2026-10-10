package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Caller must hold store.mu. One active key is allowed per Telegram account.
func (a *App) activeResellerKeyLocked(buyer int64) (string, ResellerKey) {
	for hash, key := range a.store.data.ResellerKeys {
		if key.BuyerID == buyer {
			return hash, key
		}
	}
	return "", ResellerKey{}
}
func resellerKeyVersion(hash string) string {
	if len(hash) < 16 {
		return ""
	}
	return hash[:16]
}
func (a *App) resellerKeyCommand(ctx context.Context, buyer int64, action string) {
	if buyer <= 0 {
		return
	}
	a.resellerMu.Lock()
	defer a.resellerMu.Unlock()
	action = strings.ToLower(strings.TrimSpace(action))
	a.store.mu.Lock()
	oldHash, old := a.activeResellerKeyLocked(buyer)
	a.store.mu.Unlock()
	reply := func(text string, rows [][]Button) error {
		return a.tg.send(ctx, buyer, text, &Markup{InlineKeyboard: rows})
	}
	if action == "" || action == "menu" {
		status := "No active key."
		buttons := [][]Button{{{Text: "Create API key", Data: "apikey:generate"}}}
		if oldHash != "" {
			status = "Active key ending in <code>" + esc(old.Hint) + "</code>."
			buttons = [][]Button{{{Text: "Rotate key", Data: "apikey:rotate"}, {Text: "Revoke key", Data: "apikey:revoke"}}}
		}
		buttons = append(buttons, []Button{{Text: "📖 Setup guide", Data: "apikey:help"}}, []Button{{Text: "🏠 Menu", Data: "home"}})
		_ = reply("🔑 <b>RESELLER API</b>\n\n"+status+"\n\nYour key can read prices, spend your USDT wallet balance, and retrieve your API purchases and their private delivery items. Keep it secret. Top up with /wallet.", buttons)
		return
	}
	if action == "help" {
		base := "Ask support for the API base URL."
		if u, err := url.Parse(a.cfg.MiniAppURL); err == nil && u.Host != "" {
			base = "Base URL: <code>" + esc(strings.TrimRight(a.cfg.MiniAppURL, "/")+"/api/v1/reseller") + "</code>"
		}
		_ = reply(resellerSetupGuide(base), [][]Button{{{Text: "‹ Reseller API", Data: "apikey:menu"}}})
		return
	}
	if action == "rotate" || action == "revoke" {
		if oldHash == "" {
			_ = reply("No active key. Use /apikey generate.", nil)
			return
		}
		_ = reply("Confirm "+action+"? Your current key will stop working immediately.", [][]Button{{{Text: "Confirm " + action, Data: "apikey:confirm" + action + ":" + resellerKeyVersion(oldHash)}}, {{Text: "Cancel", Data: "apikey:menu"}}})
		return
	}
	parts := strings.Split(action, ":")
	rotate := len(parts) == 2 && parts[0] == "confirmrotate"
	revoke := len(parts) == 2 && parts[0] == "confirmrevoke"
	if rotate || revoke {
		if oldHash == "" || parts[1] != resellerKeyVersion(oldHash) {
			_ = reply("This confirmation is no longer current. Open /apikey again.", nil)
			return
		}
	}
	if revoke {
		a.store.mu.Lock()
		delete(a.store.data.ResellerKeys, oldHash)
		err := a.store.saveLocked()
		a.store.mu.Unlock()
		if err != nil {
			_ = reply("Could not revoke your key. Try again.", nil)
		} else {
			_ = reply("Your API key has been revoked.", nil)
		}
		return
	}
	if action != "generate" && !rotate {
		_ = reply("Use /apikey, /apikey generate, /apikey rotate, /apikey revoke, or /apikey help.", nil)
		return
	}
	if !rotate && oldHash != "" {
		_ = reply("You already have an active key. Use /apikey rotate if you need a new one.", nil)
		return
	}
	if !a.requireChannel(ctx, buyer, buyer) {
		return
	}
	raw, key, err := newResellerKey(buyer)
	if err != nil {
		_ = reply("Could not generate your API key. Try again.", nil)
		return
	}
	// Send before activation so failed Telegram delivery cannot strand a rotated key.
	if err = reply(fmt.Sprintf("🔑 <b>Your reseller API key</b>\n<code>%s</code>\n\nStore it now; it cannot be shown again. It can spend your wallet balance and retrieve private purchased items. Tap Setup guide below for requests.", esc(raw)), [][]Button{{{Text: "📖 Setup guide", Data: "apikey:help"}}}); err != nil {
		return
	}
	a.store.mu.Lock()
	if a.store.data.ResellerKeys == nil {
		a.store.data.ResellerKeys = make(map[string]ResellerKey)
	}
	delete(a.store.data.ResellerKeys, oldHash)
	a.store.data.ResellerKeys[resellerKeyHash(raw)] = key
	err = a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		_ = reply("Key activation failed. The key just shown is invalid. Your previous key, if any, remains active. Try again.", nil)
	}
}

// Telegram renders the short guide directly in the customer's private chat.
func resellerSetupGuide(base string) string {
	return "📖 <b>RESELLER API · QUICK SETUP</b>\n\n" +
		"<b>1. Create your key</b>\nUse /apikey generate and save the key shown once. Keep it on your server; it can spend your wallet funds. Rotate or revoke it through /apikey.\n\n" +
		"<b>2. Add funds</b>\nUse /wallet in this bot to add USDT. Wait for the top-up to be confirmed before ordering.\n\n" +
		"<b>3. Connect your store</b>\n" + base + "\nReplace BASE_URL in the examples with this full URL and YOUR_KEY with your key. Send the Authorization header on every request.\n\n" +
		"<b>Check prices and stock</b>\n<pre>" + esc(`curl "BASE_URL/prices" -H "Authorization: Bearer YOUR_KEY"`) + "</pre>\nCopy the exact sku from products. priceUsdt is the unit price in USDT; stock is the available quantity. Check your funds with GET /balance.\n\n" +
		"<b>4. Place an order</b>\nReplace SKU with a catalog sku. Quantity must be 1–50. This request spends your wallet balance.\n<pre>" + esc(`curl "BASE_URL/orders" \
  -H "Authorization: Bearer YOUR_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: my-order-001" \
  --data '{"sku":"SKU","quantity":1}'`) + "</pre>\nUse a new Idempotency-Key for each new purchase. If a request times out, retry with the <b>same key and body</b> to avoid paying twice.\n\n" +
		"<b>5. Retrieve your items</b>\nA successful purchase returns order.id, order.amountUsdt, order.deliveryInstruction, and order.items (your purchased items). Save the order ID. Fetch it again with:\n<pre>" + esc(`curl "BASE_URL/orders/ORDER_ID" -H "Authorization: Bearer YOUR_KEY"`) + "</pre>\nUse GET /orders for your API order history. Delivery items appear in the purchase response and order details.\n\n" +
		"<b>Quick troubleshooting</b>\n401: check your API key.\n409: read the error code; check funds, stock, or whether the idempotency key was used for a different order.\n429: wait the Retry-After seconds before retrying.\nLimit: 60 requests per minute. Use /support if you need help."
}
