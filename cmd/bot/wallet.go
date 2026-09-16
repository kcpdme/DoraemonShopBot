package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	minWalletTopUpCents int64 = 100
	maxWalletTopUpCents int64 = 10000000
)

func walletKey(userID int64) string { return strconv.FormatInt(userID, 10) }

func formatUSDT(cents int64) string { return fmt.Sprintf("%.2f", float64(cents)/100) }

func parseUSDTAmount(raw string) (int64, error) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "$"))
	parts := strings.Split(raw, ".")
	if len(parts) > 2 || len(parts) == 0 || parts[0] == "" {
		return 0, errors.New("Enter an amount such as 10 or 12.50")
	}
	for _, part := range parts {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, errors.New("Enter a USDT amount using numbers only")
			}
		}
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > maxWalletTopUpCents/100 {
		return 0, errors.New("Enter a smaller USDT amount")
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > 2 {
		return 0, errors.New("Use no more than two decimal places")
	}
	for len(fraction) < 2 {
		fraction += "0"
	}
	cents := int64(0)
	if fraction != "" {
		cents, _ = strconv.ParseInt(fraction, 10, 64)
	}
	amount := whole*100 + cents
	if amount < minWalletTopUpCents || amount > maxWalletTopUpCents {
		return 0, fmt.Errorf("Top up between %s and %s USDT", formatUSDT(minWalletTopUpCents), formatUSDT(maxWalletTopUpCents))
	}
	return amount, nil
}

func (a *App) walletAccount(userID int64) WalletAccount {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	return a.store.data.Wallets[walletKey(userID)]
}

func (a *App) walletHome(ctx context.Context, chat, userID int64) {
	account := a.walletAccount(userID)
	var recent []WalletEntry
	if len(account.Entries) > 0 {
		start := len(account.Entries) - 3
		if start < 0 {
			start = 0
		}
		recent = account.Entries[start:]
	}
	text := fmt.Sprintf("💰 <b>MY WALLET</b>\n\n<blockquote>Available balance: <b>$%s USDT</b>\n🔒 Funds are credited only after on-chain verification.</blockquote>", formatUSDT(account.BalanceCents))
	if len(recent) > 0 {
		text += "\n\n<b>Recent activity</b>"
		for i := len(recent) - 1; i >= 0; i-- {
			entry := recent[i]
			label := "Wallet activity"
			switch entry.Kind {
			case "top_up":
				label = "➕ Verified top-up"
			case "purchase":
				label = "🛍 Purchase"
			}
			sign := "+"
			if entry.AmountCents < 0 {
				sign = "−"
			}
			text += fmt.Sprintf("\n%s %s$%s", label, sign, formatUSDT(absCents(entry.AmountCents)))
		}
	}
	text += "\n\nUse wallet funds for an instant checkout, or add USDT with BNB Smart Chain or Polygon. Wallet funds are not transferable or withdrawable through this bot."
	a.tg.send(ctx, chat, text, &Markup{InlineKeyboard: [][]Button{
		{{Text: "➕ Add USDT", Data: "wallet:topup"}},
		{{Text: "🛍 Shop with wallet", Data: "catalog"}},
		{{Text: "🏠 Menu", Data: "home"}},
	}})
}

func absCents(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (a *App) walletTopUpScreen(ctx context.Context, chat int64) {
	a.tg.send(ctx, chat, "➕ <b>ADD USDT TO WALLET</b>\n\nChoose an amount. After you submit a verified USDT transaction, the exact amount is credited to your wallet automatically.", &Markup{InlineKeyboard: [][]Button{
		{{Text: "$5", Data: "wallet:amount:500"}, {Text: "$10", Data: "wallet:amount:1000"}, {Text: "$25", Data: "wallet:amount:2500"}},
		{{Text: "$50", Data: "wallet:amount:5000"}, {Text: "$100", Data: "wallet:amount:10000"}},
		{{Text: "✏️ Custom amount", Data: "wallet:custom"}},
		{{Text: "‹ Back to wallet", Data: "wallet"}},
	}})
}

func (a *App) walletNetworkScreen(ctx context.Context, chat int64, cents int64) {
	a.tg.send(ctx, chat, fmt.Sprintf("💳 <b>SELECT TOP-UP NETWORK</b>\n\nYou are adding <b>$%s USDT</b> to your wallet. Choose the same network you will use in your wallet or exchange.", formatUSDT(cents)), &Markup{InlineKeyboard: [][]Button{
		{{Text: "🟡 USDT · BNB Smart Chain (BEP-20)", Data: fmt.Sprintf("wallet:network:bep20:%d", cents)}},
		{{Text: "🟣 USDT · Polygon", Data: fmt.Sprintf("wallet:network:polygon:%d", cents)}},
		{{Text: "‹ Change amount", Data: "wallet:topup"}},
	}})
}

func (a *App) createWalletDeposit(buyer *User, network string, cents int64) (WalletDeposit, error) {
	if buyer == nil || buyer.ID <= 0 || (network != "bep20" && network != "polygon") || cents < minWalletTopUpCents || cents > maxWalletTopUpCents {
		return WalletDeposit{}, errors.New("Choose a valid top-up amount and network")
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for _, d := range a.store.data.WalletDeposits {
		if d.BuyerID == buyer.ID && d.Network == network && d.AmountCents == cents && (d.Status == "awaiting_payment" || d.Status == "proof_invalid" || d.Status == "payment_submitted") && time.Since(d.CreatedAt) < 30*time.Minute {
			return d, nil
		}
	}
	d := WalletDeposit{ID: id("topup"), BuyerID: buyer.ID, Network: network, AmountCents: cents, Status: "awaiting_payment", CreatedAt: time.Now().UTC()}
	a.store.data.WalletDeposits[d.ID] = d
	if err := a.store.saveLocked(); err != nil {
		return WalletDeposit{}, err
	}
	return d, nil
}

func (a *App) walletDepositText(d WalletDeposit) string {
	_, network, _ := paymentNetwork(d.Network)
	address := a.cfg.BEP20Address
	if d.Network == "polygon" {
		address = a.cfg.PolygonAddress
	}
	if d.Status == "credited" {
		return fmt.Sprintf("✅ <b>WALLET TOP-UP COMPLETE</b>\n\n<blockquote>➕ $%s USDT credited\n💰 Your wallet balance is ready to use.</blockquote>\n\nTop-up: <code>%s</code>", formatUSDT(d.AmountCents), esc(d.ID))
	}
	if d.Status == "proof_invalid" {
		return fmt.Sprintf("⚠️ <b>TOP-UP NEEDS ACTION</b>\n\nTop-up: <code>%s</code>\n%s\n\nSubmit the correct transaction before the reservation expires. Your wallet is not credited until the payment is verified.", esc(d.ID), esc(d.PaymentIssue))
	}
	if d.Status == "payment_submitted" {
		issue := d.PaymentIssue
		if issue == "" {
			issue = "Your transaction is being checked."
		}
		return fmt.Sprintf("⏳ <b>VERIFYING WALLET TOP-UP</b>\n\nTop-up: <code>%s</code>\nAmount: <b>$%s USDT</b>\n🌐 %s\n\n%s\n\nYour wallet is credited automatically after 3 confirmations. Do not submit the same TxID again.", esc(d.ID), formatUSDT(d.AmountCents), esc(network), esc(issue))
	}
	if d.Status == "expired" {
		return fmt.Sprintf("⌛ <b>TOP-UP WINDOW ENDED</b>\n\nTop-up: <code>%s</code>\n\nDo not send a new payment for this top-up. If you already paid, contact support with the TxID.", esc(d.ID))
	}
	expires := d.CreatedAt.Add(30 * time.Minute)
	left := max(0, int(time.Until(expires).Seconds()))
	return fmt.Sprintf("💰 <b>ADD USDT TO WALLET</b>\n\nTop-up: <code>%s</code>\n<blockquote>💵 Send exactly <b>$%s USDT</b>\n🌐 %s\n⏳ %02d:%02d remaining</blockquote>\n\n<b>Send to this wallet</b> · tap to copy\n<code>%s</code>\n\nAfter paying, submit your TxID. The amount is credited automatically after 3 confirmations.", esc(d.ID), formatUSDT(d.AmountCents), esc(network), left/60, left%60, esc(address))
}

func walletDepositMarkup(d WalletDeposit) *Markup {
	rows := [][]Button{}
	if d.Status == "awaiting_payment" || d.Status == "proof_invalid" {
		rows = append(rows, []Button{{Text: "🧾 I've paid · Submit TxID", Data: "wallet:proof:" + d.ID}})
		rows = append(rows, []Button{{Text: "🔄 Refresh top-up", Data: "wallet:deposit:" + d.ID}})
	}
	if d.Status == "payment_submitted" {
		rows = append(rows, []Button{{Text: "🔄 Check top-up", Data: "wallet:check:" + d.ID}})
	}
	rows = append(rows, []Button{{Text: "💰 My wallet", Data: "wallet"}, {Text: "💬 Support", Data: "support"}})
	return &Markup{InlineKeyboard: rows}
}

func (a *App) sendWalletDepositCard(ctx context.Context, chat int64, d WalletDeposit, editMessageID int64) error {
	a.store.mu.Lock()
	latest, ok := a.store.data.WalletDeposits[d.ID]
	a.store.mu.Unlock()
	if ok {
		d = latest
	}
	v := map[string]any{"chat_id": chat, "text": a.walletDepositText(d), "parse_mode": "HTML", "disable_web_page_preview": true, "reply_markup": walletDepositMarkup(d)}
	messageID := editMessageID
	if messageID > 0 {
		v["message_id"] = messageID
		if err := a.tg.call(ctx, "editMessageText", v, nil); err != nil && !strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
			if !cannotEditCheckoutMessage(err) {
				return err
			}
			messageID = 0
		}
	}
	if messageID == 0 {
		delete(v, "message_id")
		var sent Message
		if err := a.tg.call(ctx, "sendMessage", v, &sent); err != nil {
			return err
		}
		messageID = sent.MessageID
	}
	if messageID > 0 {
		a.store.mu.Lock()
		deferred := a.store.data.WalletDeposits[d.ID]
		if deferred.PaymentMessageID != messageID {
			deferred.PaymentMessageID = messageID
			a.store.data.WalletDeposits[d.ID] = deferred
			err := a.store.saveLocked()
			a.store.mu.Unlock()
			return err
		}
		a.store.mu.Unlock()
	}
	return nil
}

func (a *App) beginWalletProof(ctx context.Context, chat, buyer int64, depositID string) {
	a.store.mu.Lock()
	d, ok := a.store.data.WalletDeposits[depositID]
	a.store.mu.Unlock()
	if !ok || d.BuyerID != buyer || (d.Status != "awaiting_payment" && d.Status != "proof_invalid" && d.Status != "payment_submitted") {
		a.tg.send(ctx, chat, "This top-up can no longer accept a TxID. Contact support if you paid.", nil)
		return
	}
	a.setFlow(chat, flow{Kind: "wallet_proof", SKU: d.ID})
	a.tg.send(ctx, chat, "🧾 <b>PASTE YOUR TOP-UP TXID</b>\n\nTop-up: <code>"+esc(d.ID)+"</code>\n\nPaste the full transaction hash or a BscScan / PolygonScan transaction link. Your wallet is credited only after it is verified on-chain.", &Markup{InlineKeyboard: [][]Button{{{Text: "‹ Back to top-up", Data: "wallet:deposit:" + d.ID}}}})
}

func (a *App) txHashInUseLocked(network, hash, except string) bool {
	for _, o := range a.store.data.Orders {
		if o.ID != except && o.Network == network && o.TxHash != "" && strings.EqualFold(o.TxHash, hash) {
			return true
		}
	}
	for _, d := range a.store.data.WalletDeposits {
		if d.ID != except && d.Network == network && d.TxHash != "" && strings.EqualFold(d.TxHash, hash) {
			return true
		}
	}
	return false
}

func (a *App) submitWalletTxHash(ctx context.Context, chat, buyer int64, depositID, hash string) {
	hash = normalizeHash(hash)
	if !validHash(hash) {
		a.tg.send(ctx, chat, "That TxID is incomplete. Paste all 66 characters starting with 0x, or the full transaction link.", nil)
		return
	}
	a.opMu.Lock()
	a.store.mu.Lock()
	d, ok := a.store.data.WalletDeposits[depositID]
	if !ok || d.BuyerID != buyer || (d.Status != "awaiting_payment" && d.Status != "proof_invalid" && d.Status != "payment_submitted") {
		a.store.mu.Unlock()
		a.opMu.Unlock()
		a.tg.send(ctx, chat, "This top-up cannot accept a TxID. Contact support if you paid.", nil)
		return
	}
	if d.Status != "payment_submitted" && time.Since(d.CreatedAt) >= 30*time.Minute {
		a.store.mu.Unlock()
		a.opMu.Unlock()
		a.expireWalletDeposits(ctx)
		a.tg.send(ctx, chat, "The top-up window has closed. If you already paid, contact support with the TxID. Do not pay again.", nil)
		return
	}
	if a.txHashInUseLocked(d.Network, hash, d.ID) {
		a.store.mu.Unlock()
		a.opMu.Unlock()
		a.tg.send(ctx, chat, "That transaction is already attached to another payment or top-up.", nil)
		return
	}
	d.TxHash, d.Status, d.PaymentIssue = hash, "payment_submitted", "Waiting for blockchain confirmation."
	a.store.data.WalletDeposits[d.ID] = d
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	a.opMu.Unlock()
	if err != nil {
		a.tg.send(ctx, chat, "Could not save the TxID. Please try again.", nil)
		return
	}
	a.clearFlow(chat)
	_ = a.sendWalletDepositCard(ctx, chat, d, d.PaymentMessageID)
	go a.verifyWalletDeposit(context.Background(), d.ID)
}

func (a *App) verifyWalletDeposit(ctx context.Context, depositID string) {
	key := "wallet-deposit:" + depositID
	if _, busy := a.verifying.LoadOrStore(key, true); busy {
		return
	}
	defer a.verifying.Delete(key)
	a.store.mu.Lock()
	d, ok := a.store.data.WalletDeposits[depositID]
	a.store.mu.Unlock()
	if !ok || d.Status != "payment_submitted" {
		return
	}
	probe := Order{Amount: float64(d.AmountCents) / 100, Network: d.Network, TxHash: d.TxHash, CreatedAt: d.CreatedAt}
	verified, reason, err := a.checkPayment(ctx, probe)
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.store.mu.Lock()
	latest, exists := a.store.data.WalletDeposits[depositID]
	if !exists || latest.Status != "payment_submitted" || latest.TxHash != d.TxHash {
		a.store.mu.Unlock()
		return
	}
	d = latest
	if isPermanentPaymentError(err) {
		d.Status, d.PaymentIssue = "proof_invalid", reason
		if d.PaymentIssue == "" {
			d.PaymentIssue = err.Error()
		}
	} else if err != nil {
		d.PaymentIssue = "The network is temporarily unavailable. Your TxID is saved; we’ll retry automatically."
	} else if !verified {
		d.PaymentIssue = reason
	} else {
		d.Status, d.PaymentIssue, d.CreditedAt = "credited", "", time.Now().UTC()
		account := a.store.data.Wallets[walletKey(d.BuyerID)]
		account.BalanceCents += d.AmountCents
		account.Entries = append(account.Entries, WalletEntry{ID: id("ledger"), Kind: "top_up", Reference: d.ID, AmountCents: d.AmountCents, CreatedAt: d.CreditedAt})
		a.store.data.Wallets[walletKey(d.BuyerID)] = account
	}
	a.store.data.WalletDeposits[d.ID] = d
	saveErr := a.store.saveLocked()
	a.store.mu.Unlock()
	if saveErr != nil {
		return
	}
	if d.Status == "proof_invalid" {
		_ = a.tg.send(ctx, d.BuyerID, "❌ <b>TOP-UP TXID NOT ACCEPTED</b>\n\n"+esc(d.PaymentIssue)+"\n\nSubmit the correct TxID or contact support. Do not send another payment just to retry verification.", walletDepositMarkup(d))
	}
	_ = a.sendWalletDepositCard(ctx, d.BuyerID, d, d.PaymentMessageID)
}

func (a *App) purchaseWithWallet(ctx context.Context, buyer *User, sku string, quantity int) (Order, error) {
	o, err := a.createOrderQuantity(buyer, sku, "wallet", quantity)
	if err != nil {
		return Order{}, err
	}
	cost := int64(o.Amount*100 + 0.5)
	a.opMu.Lock()
	a.store.mu.Lock()
	latest := a.store.data.Orders[o.ID]
	if latest.Status != "awaiting_payment" {
		a.store.mu.Unlock()
		a.opMu.Unlock()
		return Order{}, errors.New("This wallet order is already being processed")
	}
	account := a.store.data.Wallets[walletKey(buyer.ID)]
	if account.BalanceCents < cost {
		for _, stockID := range latest.StockIDs {
			stock := a.store.data.Stock[stockID]
			if stock.OrderID == latest.ID && !stock.Sold {
				stock.OrderID = ""
				a.store.data.Stock[stockID] = stock
			}
		}
		latest.Status = "cancelled"
		a.store.data.Orders[latest.ID] = latest
		_ = a.store.saveLocked()
		a.store.mu.Unlock()
		a.opMu.Unlock()
		return Order{}, fmt.Errorf("Your wallet balance is $%s USDT. Add funds to complete this $%s purchase", formatUSDT(account.BalanceCents), formatUSDT(cost))
	}
	account.BalanceCents -= cost
	account.Entries = append(account.Entries, WalletEntry{ID: id("ledger"), Kind: "purchase", Reference: latest.ID, AmountCents: -cost, CreatedAt: time.Now().UTC()})
	a.store.data.Wallets[walletKey(buyer.ID)] = account
	latest.Status, latest.PaymentIssue, latest.PaidAt = "delivery_pending", "Paid from wallet. Sending your items now.", time.Now().UTC()
	a.store.data.Orders[latest.ID] = latest
	err = a.store.saveLocked()
	a.store.mu.Unlock()
	a.opMu.Unlock()
	if err != nil {
		return Order{}, err
	}
	if err := a.deliverOrder(ctx, latest.ID); err != nil {
		return latest, err
	}
	return latest, nil
}

func (a *App) expireWalletDeposits(ctx context.Context) {
	if !a.opMu.TryLock() {
		return
	}
	defer a.opMu.Unlock()
	a.store.mu.Lock()
	expired := []WalletDeposit{}
	for id, d := range a.store.data.WalletDeposits {
		if (d.Status == "awaiting_payment" || d.Status == "proof_invalid") && time.Since(d.CreatedAt) >= 30*time.Minute {
			d.Status = "expired"
			a.store.data.WalletDeposits[id] = d
			expired = append(expired, d)
		}
	}
	var err error
	if len(expired) > 0 {
		err = a.store.saveLocked()
	}
	a.store.mu.Unlock()
	if err != nil {
		return
	}
	for _, d := range expired {
		_ = a.sendWalletDepositCard(ctx, d.BuyerID, d, d.PaymentMessageID)
	}
}

func (a *App) handleWalletCallback(ctx context.Context, c *Callback, chat int64) bool {
	switch {
	case c.Data == "wallet:topup":
		a.walletTopUpScreen(ctx, chat)
	case c.Data == "wallet:custom":
		a.setFlow(chat, flow{Kind: "wallet_amount"})
		a.tg.send(ctx, chat, "✏️ <b>CUSTOM TOP-UP AMOUNT</b>\n\nSend a USDT amount between $1.00 and $100,000.00, for example <code>12.50</code>.", &Markup{InlineKeyboard: [][]Button{{{Text: "‹ Back", Data: "wallet:topup"}}}})
	case strings.HasPrefix(c.Data, "wallet:amount:"):
		cents, err := strconv.ParseInt(strings.TrimPrefix(c.Data, "wallet:amount:"), 10, 64)
		if err != nil || cents < minWalletTopUpCents || cents > maxWalletTopUpCents {
			a.tg.send(ctx, chat, "Choose a valid top-up amount.", nil)
			return true
		}
		a.walletNetworkScreen(ctx, chat, cents)
	case strings.HasPrefix(c.Data, "wallet:network:"):
		parts := strings.Split(c.Data, ":")
		if len(parts) != 4 {
			return true
		}
		cents, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			a.tg.send(ctx, chat, "Choose a valid top-up amount.", nil)
			return true
		}
		d, err := a.createWalletDeposit(&c.From, parts[2], cents)
		if err != nil {
			a.tg.send(ctx, chat, esc(err.Error()), nil)
			return true
		}
		_ = a.sendWalletDepositCard(ctx, chat, d, 0)
	case strings.HasPrefix(c.Data, "wallet:deposit:"):
		id := strings.TrimPrefix(c.Data, "wallet:deposit:")
		a.store.mu.Lock()
		d, ok := a.store.data.WalletDeposits[id]
		a.store.mu.Unlock()
		if !ok || d.BuyerID != c.From.ID {
			a.tg.send(ctx, chat, "Top-up not found.", nil)
			return true
		}
		_ = a.sendWalletDepositCard(ctx, chat, d, d.PaymentMessageID)
	case strings.HasPrefix(c.Data, "wallet:proof:"):
		a.beginWalletProof(ctx, chat, c.From.ID, strings.TrimPrefix(c.Data, "wallet:proof:"))
	case strings.HasPrefix(c.Data, "wallet:check:"):
		id := strings.TrimPrefix(c.Data, "wallet:check:")
		go a.verifyWalletDeposit(context.Background(), id)
	case strings.HasPrefix(c.Data, "walletbuy:"):
		parts := strings.Split(c.Data, ":")
		if len(parts) != 3 {
			return true
		}
		quantity, err := strconv.Atoi(parts[2])
		if err != nil {
			return true
		}
		o, err := a.purchaseWithWallet(ctx, &c.From, parts[1], quantity)
		if err != nil {
			a.tg.send(ctx, chat, "💰 "+esc(err.Error()), &Markup{InlineKeyboard: [][]Button{{{Text: "➕ Add USDT", Data: "wallet:topup"}, {Text: "💰 My wallet", Data: "wallet"}}}})
			return true
		}
		_ = a.tg.send(ctx, chat, "💰 <b>PAID FROM WALLET</b>\n\n<blockquote>− $"+formatUSDT(int64(o.Amount*100+0.5))+" USDT\n✅ Your order is being delivered in this chat.</blockquote>", &Markup{InlineKeyboard: [][]Button{{{Text: "💰 My wallet", Data: "wallet"}, {Text: "📦 My orders", Data: "orders"}}}})
	default:
		return false
	}
	return true
}
