package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxCheckoutQuantity = 50

// Serialize card navigation with background edits so a countdown that was
// already in flight cannot overwrite the screen the customer just selected.
var checkoutMessageMu sync.Mutex

func cannotEditCheckoutMessage(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "message to edit not found") || strings.Contains(message, "message can't be edited") || strings.Contains(message, "message cannot be edited")
}

func (a *App) detachPaymentMessage(chat, messageID int64) error {
	if messageID <= 0 {
		return nil
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	changed := false
	for id, order := range a.store.data.Orders {
		if order.BuyerID == chat && order.PaymentMessageID == messageID {
			order.PaymentMessageID = 0
			a.store.data.Orders[id] = order
			changed = true
		}
	}
	if changed {
		return a.store.saveLocked()
	}
	return nil
}

func checkoutNavigation() *Markup {
	return &Markup{InlineKeyboard: [][]Button{{{Text: "🛍 Browse shop", Data: "catalog"}, {Text: "💬 Support", Data: "support"}}}}
}

// checkoutScreen keeps a button-driven checkout in one message. Old messages and
// text-entry steps still work if Telegram can no longer edit the original card.
func (a *App) checkoutScreen(ctx context.Context, chat, messageID int64, text string, kb *Markup) error {
	checkoutMessageMu.Lock()
	defer checkoutMessageMu.Unlock()
	if messageID > 0 {
		if err := a.detachPaymentMessage(chat, messageID); err != nil {
			return err
		}
		err := a.tg.call(ctx, "editMessageText", map[string]any{"chat_id": chat, "message_id": messageID, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true, "reply_markup": kb}, nil)
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
			return nil
		}
		if !cannotEditCheckoutMessage(err) {
			return err
		}
	}
	return a.tg.send(ctx, chat, text, kb)
}

func (a *App) productText(code string) (string, *Markup) {
	a.store.mu.Lock()
	p, ok := a.store.data.Products[code]
	stock := 0
	for _, item := range a.store.data.Stock {
		if item.SKU == code && !item.Sold && item.OrderID == "" {
			stock++
		}
	}
	a.store.mu.Unlock()
	if !ok || !p.Active {
		return "This product is no longer available. Explore the shop for something else.", checkoutNavigation()
	}
	text := fmt.Sprintf("📦 <b>%s</b>\n\n%s\n\n💵 <b>%.2f USDT</b> per item\n✅ Available: <b>%d</b>\n\n⚡ Your digital items and delivery instructions arrive here after payment is confirmed.", esc(p.Name), esc(p.Description), p.PriceUSDT, stock)
	rows := [][]Button{}
	if stock > 0 {
		rows = append(rows, []Button{{Text: "🛒 Buy now", Data: "buy:" + code}})
	} else {
		text += "\n\n<b>Currently sold out.</b> Restocks are announced in the updates channel."
	}
	rows = append(rows, []Button{{Text: "‹ Back to shop", Data: "catalog"}, {Text: "💬 Support", Data: "support"}})
	return text, &Markup{InlineKeyboard: rows}
}

func (a *App) checkoutProduct(sku string, qty int) (Product, int, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	p, ok := a.store.data.Products[sku]
	if !ok || !p.Active {
		return Product{}, 0, errors.New("this product is no longer available")
	}
	stock := 0
	for _, item := range a.store.data.Stock {
		if item.SKU == sku && !item.Sold && item.OrderID == "" {
			stock++
		}
	}
	if stock == 0 {
		return p, stock, errors.New("this product has sold out; choose another product or watch the updates channel for restocks")
	}
	if qty < 1 || qty > maxCheckoutQuantity {
		return p, stock, fmt.Errorf("choose a whole number from 1 to %d", min(stock, maxCheckoutQuantity))
	}
	if qty > stock {
		return p, stock, fmt.Errorf("only %d items are available; choose a smaller quantity", stock)
	}
	return p, stock, nil
}

func (a *App) quantityText(sku string) (string, *Markup) {
	p, stock, err := a.checkoutProduct(sku, 1)
	if err != nil {
		return "🛍 " + esc(err.Error()), checkoutNavigation()
	}
	limit := min(stock, maxCheckoutQuantity)
	rows := [][]Button{}
	row := []Button{}
	for _, qty := range []int{1, 2, 3, 5, 10} {
		if qty > limit {
			continue
		}
		row = append(row, Button{Text: fmt.Sprintf("%d × · %.2f USDT", qty, p.PriceUSDT*float64(qty)), Data: fmt.Sprintf("qty:%s:%d", sku, qty)})
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	if limit > 1 {
		rows = append(rows, []Button{{Text: "✏️ Enter quantity", Data: "qty:" + sku + ":custom"}})
	}
	rows = append(rows, []Button{{Text: "‹ Product details", Data: "product:" + sku}, {Text: "🏠 Menu", Data: "home"}})
	return fmt.Sprintf("🛍 <b>Choose your quantity</b>\n<i>Step 1 of 3 · Quantity</i>\n\n📦 %s\n💵 Unit price: <b>%.2f USDT</b>\n✅ Available: <b>%d</b>\n\nHow many would you like?", esc(p.Name), p.PriceUSDT, stock), &Markup{InlineKeyboard: rows}
}

func (a *App) networkChoice(sku string, qty int) (string, *Markup, error) {
	p, _, err := a.checkoutProduct(sku, qty)
	if err != nil {
		return "", nil, err
	}
	text := fmt.Sprintf("💳 <b>Choose your payment network</b>\n<i>Step 2 of 3 · Network</i>\n\n📦 %s\n🔢 Quantity: <b>%d</b>\n💵 Total: <b>%.2f USDT</b>\n\nSelect the same network you will use in your wallet or exchange. Your stock is reserved when you continue.", esc(p.Name), qty, p.PriceUSDT*float64(qty))
	kb := &Markup{InlineKeyboard: [][]Button{
		{{Text: "🟡 USDT · BNB Smart Chain (BEP-20)", Data: fmt.Sprintf("network:bep20:%s:%d", sku, qty)}},
		{{Text: "🟣 USDT · Polygon", Data: fmt.Sprintf("network:polygon:%s:%d", sku, qty)}},
		{{Text: "‹ Change quantity", Data: "buy:" + sku}, {Text: "💬 Support", Data: "support"}},
	}}
	return text, kb, nil
}

func (a *App) showNetworkChoice(ctx context.Context, chat int64, sku string, qty int) error {
	text, kb, err := a.networkChoice(sku, qty)
	if err != nil {
		return err
	}
	a.clearFlow(chat)
	return a.checkoutScreen(ctx, chat, 0, text, kb)
}

func paymentNetwork(network string) (string, string, string) {
	if network == "polygon" {
		return "🟣 USDT · Polygon", "Polygon PoS", "https://polygonscan.com/tx/"
	}
	return "🟡 USDT · BEP-20", "BNB Smart Chain (BEP-20)", "https://bscscan.com/tx/"
}

func (a *App) paymentText(o Order) string {
	a.store.mu.Lock()
	product := a.store.data.Products[o.SKU]
	a.store.mu.Unlock()
	name := o.ProductName
	if name == "" {
		name = product.Name
	}
	if name == "" {
		name = o.SKU
	}
	title, network, _ := paymentNetwork(o.Network)
	quantity := o.Quantity
	if quantity < 1 {
		quantity = 1
	}
	summary := fmt.Sprintf("📦 %s × %d\n🧾 Order: <code>%s</code>\n💵 Total: <b>%.2f USDT</b>", esc(name), quantity, esc(o.ID), o.Amount)
	if o.Status == "proof_invalid" {
		text := "⚠️ <b>This transaction cannot pay for this order</b>\n\n" + summary + "\n\n" + esc(o.PaymentIssue)
		if time.Now().Before(o.CreatedAt.Add(30 * time.Minute)) {
			text += "\n\n<b>Automatic checks have stopped for this TxID.</b> Submit the correct transaction below. If you need help finding it, contact support."
		} else {
			text += "\n\nThe reservation window has ended. If you already paid, contact support with your order ID and TxID."
		}
		return text
	}
	if o.Status != "awaiting_payment" && o.Status != "payment_submitted" {
		text := "🧾 <b>" + esc(friendlyOrderStatus(o.Status)) + "</b>\n\n" + summary
		switch o.Status {
		case "delivered":
			text += "\n\n✅ Your items and instructions were sent in this chat. Need a hand? Contact support below."
		case "delivery_pending":
			text += "\n\n✅ Payment is confirmed. Your items are being delivered here. You do not need to pay again."
		default:
			text += "\n\nThis checkout is closed. If you already sent a payment, contact support with this order ID and your TxID."
		}
		return text
	}
	if o.Status == "payment_submitted" {
		issue := strings.TrimSpace(o.PaymentIssue)
		if issue == "" {
			issue = "Your transaction is being checked."
		}
		return "⏳ <b>Checking your payment</b>\n\n" + summary + fmt.Sprintf("\n🌐 %s\n\nTxID:\n<code>%s</code>\n\n<b>Latest update</b>\n%s\n\nWe check pending transactions every 30 seconds. If this transaction cannot pay for this order, we will explain why and let you submit the correct TxID.\n\nOnce verified, your digital items and delivery instructions arrive here automatically. <b>Do not pay again while verification is pending.</b>", esc(network), esc(o.TxHash), esc(issue))
	}
	addr := a.cfg.BEP20Address
	if o.Network == "polygon" {
		addr = a.cfg.PolygonAddress
	}
	expires := o.CreatedAt.Add(30 * time.Minute)
	if !time.Now().Before(expires) {
		return "⌛ <b>Payment window ended</b>\n\n" + summary + "\n\nDo not send a new payment to this order. If you already paid, contact support with your TxID. Otherwise, return to the shop to start a fresh checkout."
	}
	secondsLeft := max(0, int(time.Until(expires).Seconds()))
	return fmt.Sprintf("%s\n<i>Step 3 of 3 · Pay &amp; receive</i>\n\n%s\n\n<b>Send exactly</b>\n<blockquote><b>%.2f USDT</b></blockquote>\n\n<b>To this wallet</b> · tap to copy\n<code>%s</code>\n\n🌐 Network: <b>%s</b>\n⏳ Time left: <b>%02d:%02d</b> · ends %s UTC\n\n1. Send USDT on the network above. The wallet must receive the exact amount after withdrawal fees.\n2. Tap <b>I've paid · Submit TxID</b> and paste your transaction hash or explorer link.\n3. Receive your items here after <b>3 confirmations</b>.\n\nHave your TxID ready before the reservation ends.", "<b>"+esc(title)+"</b>", summary, o.Amount, esc(addr), esc(network), secondsLeft/60, secondsLeft%60, expires.UTC().Format("15:04"))
}

func paymentMarkup(o Order) *Markup {
	rows := [][]Button{}
	active := (o.Status == "awaiting_payment" || o.Status == "proof_invalid") && time.Now().Before(o.CreatedAt.Add(30*time.Minute))
	if active {
		label := "✅ I've paid · Submit TxID"
		if o.Status == "proof_invalid" {
			label = "🧾 Submit a different TxID"
		}
		rows = append(rows, []Button{{Text: label, Data: "proof:" + o.ID}})
		rows = append(rows, []Button{{Text: "🔄 Refresh payment", Data: "pay:" + o.ID}})
	}
	if o.Status == "payment_submitted" {
		rows = append(rows, []Button{{Text: "🔄 Check payment now", Data: "check:" + o.ID}})
		rows = append(rows, []Button{{Text: "🧾 Correct my TxID", Data: "proof:" + o.ID}})
	}
	if validHash(o.TxHash) {
		_, _, explorer := paymentNetwork(o.Network)
		rows = append(rows, []Button{{Text: "🔎 View transaction", URL: explorer + o.TxHash}})
	}
	rows = append(rows, []Button{{Text: "📦 My orders", Data: "orders"}, {Text: "💬 Support", Data: "support"}})
	if active {
		rows = append(rows, []Button{{Text: "✕ Cancel unpaid order", Data: "cancel:" + o.ID}})
	} else if o.Status != "payment_submitted" && o.Status != "delivery_pending" {
		rows = append(rows, []Button{{Text: "🛍 Back to shop", Data: "catalog"}})
	}
	return &Markup{InlineKeyboard: rows}
}

func friendlyOrderStatus(s string) string {
	switch s {
	case "awaiting_payment":
		return "Awaiting payment"
	case "payment_submitted":
		return "Checking payment"
	case "delivery_pending":
		return "Payment confirmed · Delivering"
	case "delivered":
		return "Delivered"
	case "payment_rejected":
		return "Payment rejected"
	case "proof_invalid":
		return "Action needed · Check your TxID"
	case "expired":
		return "Reservation expired"
	case "cancelled":
		return "Cancelled"
	default:
		return s
	}
}

func (a *App) sendPaymentCard(ctx context.Context, chat int64, o Order, editMessageID int64) error {
	checkoutMessageMu.Lock()
	defer checkoutMessageMu.Unlock()
	a.store.mu.Lock()
	latest, exists := a.store.data.Orders[o.ID]
	a.store.mu.Unlock()
	if exists {
		// A background job may have captured this card before navigation detached
		// it or before another card replaced it. Do not reclaim that message.
		if editMessageID > 0 && o.PaymentMessageID == editMessageID && latest.PaymentMessageID != editMessageID {
			return nil
		}
		o = latest
	}
	v := map[string]any{"chat_id": chat, "text": a.paymentText(o), "parse_mode": "HTML", "disable_web_page_preview": true, "reply_markup": paymentMarkup(o)}
	messageID := editMessageID
	if messageID > 0 {
		v["message_id"] = messageID
		err := a.tg.call(ctx, "editMessageText", v, nil)
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
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
		defer a.store.mu.Unlock()
		latest, ok := a.store.data.Orders[o.ID]
		if ok && latest.PaymentMessageID != messageID {
			latest.PaymentMessageID = messageID
			a.store.data.Orders[o.ID] = latest
			return a.store.saveLocked()
		}
	}
	return nil
}

func (a *App) handleCheckoutCallback(ctx context.Context, c *Callback, chat int64) bool {
	messageID := int64(0)
	if c.Message != nil {
		messageID = c.Message.MessageID
	}
	screen := func(text string, kb *Markup) { _ = a.checkoutScreen(ctx, chat, messageID, text, kb) }
	switch {
	case c.Data == "catalog":
		a.clearFlow(chat)
		text, kb := a.productsText()
		if kb == nil {
			kb = checkoutNavigation()
		}
		screen(text, kb)
	case strings.HasPrefix(c.Data, "product:"):
		a.clearFlow(chat)
		text, kb := a.productText(strings.TrimPrefix(c.Data, "product:"))
		screen(text, kb)
	case strings.HasPrefix(c.Data, "buy:"):
		a.clearFlow(chat)
		text, kb := a.quantityText(strings.TrimPrefix(c.Data, "buy:"))
		screen(text, kb)
	case strings.HasPrefix(c.Data, "qty:"):
		parts := strings.Split(c.Data, ":")
		if len(parts) != 3 {
			screen("Please open the product again to choose a quantity.", checkoutNavigation())
			return true
		}
		if parts[2] == "custom" {
			p, stock, err := a.checkoutProduct(parts[1], 1)
			if err != nil {
				screen(esc(err.Error()), checkoutNavigation())
				return true
			}
			a.setFlow(chat, flow{Kind: "quantity", SKU: parts[1]})
			screen(fmt.Sprintf("✏️ <b>Enter a quantity</b>\n\n📦 %s\n💵 %.2f USDT per item\n\nSend a whole number from <b>1 to %d</b>.\nUse /cancel to stop.", esc(p.Name), p.PriceUSDT, min(stock, maxCheckoutQuantity)), &Markup{InlineKeyboard: [][]Button{{{Text: "‹ Quantity options", Data: "buy:" + parts[1]}}}})
			return true
		}
		qty, err := strconv.Atoi(parts[2])
		if err != nil {
			screen("Please choose a whole-number quantity.", checkoutNavigation())
			return true
		}
		text, kb, err := a.networkChoice(parts[1], qty)
		if err != nil {
			screen(esc(err.Error()), checkoutNavigation())
			return true
		}
		a.clearFlow(chat)
		screen(text, kb)
	case strings.HasPrefix(c.Data, "network:"):
		parts := strings.Split(c.Data, ":")
		if len(parts) != 3 && len(parts) != 4 {
			screen("Please start checkout again from the product.", checkoutNavigation())
			return true
		}
		qty := 1 // Keep already-issued single-item buttons usable.
		if len(parts) == 4 {
			var err error
			qty, err = strconv.Atoi(parts[3])
			if err != nil {
				screen("Please select your quantity again.", checkoutNavigation())
				return true
			}
		}
		if parts[1] != "bep20" && parts[1] != "polygon" {
			screen("Please choose BNB Smart Chain or Polygon.", checkoutNavigation())
			return true
		}
		o, err := a.createOrderQuantity(&c.From, parts[2], parts[1], qty)
		if err != nil {
			screen("We couldn't reserve those items. "+esc(err.Error()), checkoutNavigation())
			return true
		}
		a.clearFlow(chat)
		_ = a.sendPaymentCard(ctx, chat, o, messageID)
	case strings.HasPrefix(c.Data, "pay:"):
		a.store.mu.Lock()
		o, ok := a.store.data.Orders[strings.TrimPrefix(c.Data, "pay:")]
		a.store.mu.Unlock()
		if !ok || o.BuyerID != c.From.ID {
			screen("Order not found.", checkoutNavigation())
			return true
		}
		a.clearFlow(chat)
		_ = a.sendPaymentCard(ctx, chat, o, messageID)
	default:
		return false
	}
	return true
}
