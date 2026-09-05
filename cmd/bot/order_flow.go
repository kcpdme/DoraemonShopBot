package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

func proofAllowed(o Order) bool {
	return o.Status == "awaiting_payment" || o.Status == "proof_invalid" || o.Status == "payment_submitted"
}
func (a *App) createOrder(buyer *User, code, network string) (Order, error) {
	return a.createOrderQuantity(buyer, code, network, 1)
}
func (a *App) createOrderQuantity(buyer *User, code, network string, quantity int) (Order, error) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if buyer == nil || buyer.ID <= 0 || (network != "bep20" && network != "polygon") || quantity < 1 || quantity > 50 {
		return Order{}, errors.New("Choose a valid network and a quantity between 1 and 50.")
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	p, ok := a.store.data.Products[code]
	if !ok || !p.Active {
		return Order{}, errors.New("This product is unavailable.")
	}
	cents := math.Round(p.PriceUSDT * 100)
	if math.IsNaN(cents) || math.IsInf(cents, 0) || cents < 1 || cents > 1e9 {
		return Order{}, errors.New("This product's price needs owner attention.")
	}
	// Repeated taps resume the customer's existing reservation instead of consuming more stock.
	for _, o := range a.store.data.Orders {
		if o.BuyerID == buyer.ID && o.SKU == code && o.Network == network && o.Quantity == quantity && proofAllowed(o) && time.Since(o.CreatedAt) < 30*time.Minute {
			return o, nil
		}
	}
	active := 0
	for _, o := range a.store.data.Orders {
		if o.BuyerID == buyer.ID && proofAllowed(o) {
			active++
		}
	}
	if active >= 3 {
		return Order{}, errors.New("You already have 3 open orders. Complete or cancel one in My orders first.")
	}
	candidates := []StockItem{}
	for _, x := range a.store.data.Stock {
		if x.SKU == code && !x.Sold && x.OrderID == "" {
			candidates = append(candidates, x)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].AddedAt.Equal(candidates[j].AddedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].AddedAt.Before(candidates[j].AddedAt)
	})
	if len(candidates) < quantity {
		return Order{}, fmt.Errorf("Only %d units remain. Choose a smaller quantity.", len(candidates))
	}
	o := Order{ID: id("ord"), SKU: code, Quantity: quantity, BuyerID: buyer.ID, BuyerName: userLabel(buyer), Amount: cents * float64(quantity) / 100, Network: network, Status: "awaiting_payment", CreatedAt: time.Now().UTC(), ProductName: p.Name, DeliveryInstruction: p.DeliveryInstruction}
	for _, x := range candidates[:quantity] {
		x.OrderID = o.ID
		a.store.data.Stock[x.ID] = x
		o.StockIDs = append(o.StockIDs, x.ID)
	}
	a.store.data.Orders[o.ID] = o
	return o, a.store.saveLocked()
}

func (a *App) submitTxHash(ctx context.Context, chat, buyer int64, orderID, hash string) {
	hash = normalizeHash(hash)
	if !validHash(hash) {
		_ = a.tg.send(ctx, chat, "That TxID is incomplete. Paste all 66 characters starting with 0x, or the full BscScan / PolygonScan transaction link.", nil)
		return
	}
	a.opMu.Lock()
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	if !ok || o.BuyerID != buyer || !proofAllowed(o) {
		a.store.mu.Unlock()
		a.opMu.Unlock()
		_ = a.tg.send(ctx, chat, "This order cannot accept a TxID. Open My orders or contact support if you paid.", nil)
		return
	}
	if o.Status != "payment_submitted" && time.Since(o.CreatedAt) >= 30*time.Minute {
		a.store.mu.Unlock()
		a.opMu.Unlock()
		a.expireUnpaidOrders(ctx)
		_ = a.tg.send(ctx, chat, "The payment window has closed. If you already sent funds, contact support with this order and TxID. Do not pay again.", nil)
		return
	}
	for _, other := range a.store.data.Orders {
		if other.ID != o.ID && other.Network == o.Network && strings.EqualFold(other.TxHash, hash) && (other.Status == "payment_submitted" || other.Status == "delivery_pending" || other.Status == "delivered") {
			a.store.mu.Unlock()
			a.opMu.Unlock()
			_ = a.tg.send(ctx, chat, "That transaction is already attached to another order. Contact support if this is unexpected.", nil)
			return
		}
	}
	o.TxHash = hash
	o.Status = "payment_submitted"
	o.PaymentIssue = "Waiting for blockchain confirmation."
	a.store.data.Orders[o.ID] = o
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	a.opMu.Unlock()
	if err != nil {
		_ = a.tg.send(ctx, chat, "Could not save the TxID. Please try again.", nil)
		return
	}
	a.clearFlow(chat)
	_ = a.sendPaymentCard(ctx, chat, o, o.PaymentMessageID)
	go a.verifyOrder(context.Background(), o.ID)
}

func (a *App) beginProof(ctx context.Context, chat, buyer int64, orderID string) {
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	a.store.mu.Unlock()
	if !ok || o.BuyerID != buyer || !proofAllowed(o) {
		_ = a.tg.send(ctx, chat, "This order cannot accept another TxID. Contact support if you paid.", nil)
		return
	}
	a.setFlow(chat, flow{Kind: "proof", SKU: o.ID})
	_ = a.tg.send(ctx, chat, "🧾 <b>Paste your transaction</b>\n\nOrder: <code>"+esc(o.ID)+"</code>\n\nOpen your wallet's withdrawal receipt and copy the <b>TxID / transaction hash</b>. Paste it here, or send the full BscScan / PolygonScan transaction link.\n\nA screenshot or exchange withdrawal number cannot be verified on-chain.", &Markup{InlineKeyboard: [][]Button{{{Text: "↩ Back to payment", Data: "pay:" + o.ID}, {Text: "💬 Support", Data: "support"}}}})
}

func (a *App) checkOrderNow(ctx context.Context, chat, buyer int64, orderID string) {
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	a.store.mu.Unlock()
	if !ok || o.BuyerID != buyer {
		_ = a.tg.send(ctx, chat, "Order not found.", nil)
		return
	}
	if o.Status == "awaiting_payment" {
		a.beginProof(ctx, chat, buyer, orderID)
		return
	}
	_ = a.sendPaymentCard(ctx, chat, o, o.PaymentMessageID)
	if o.Status == "payment_submitted" || o.Status == "delivery_pending" {
		go a.verifyOrder(context.Background(), o.ID)
	}
}

func (a *App) verifyOrder(ctx context.Context, orderID string) {
	if _, busy := a.verifying.LoadOrStore(orderID, true); busy {
		return
	}
	defer a.verifying.Delete(orderID)
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	a.store.mu.Unlock()
	if !ok {
		return
	}
	if o.Status == "delivery_pending" {
		a.opMu.Lock()
		defer a.opMu.Unlock()
		_ = a.deliverLocked(ctx, orderID)
		return
	}
	if o.Status != "payment_submitted" {
		return
	}
	verified, reason, err := a.checkPayment(ctx, o)
	// RPC can take seconds. Do not block other buyers, and discard results if
	// the customer replaced the proof or the owner resolved the order meanwhile.
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.store.mu.Lock()
	latest := a.store.data.Orders[orderID]
	a.store.mu.Unlock()
	if latest.Status != "payment_submitted" || latest.TxHash != o.TxHash {
		return
	}
	o = latest
	if isPermanentPaymentError(err) {
		o.Status = "proof_invalid"
		o.PaymentIssue = reason
		if o.PaymentIssue == "" {
			o.PaymentIssue = err.Error()
		}
	} else if err != nil {
		o.PaymentIssue = "The network is temporarily unavailable. Your TxID is saved; we’ll retry automatically."
	} else if !verified {
		o.PaymentIssue = reason
	} else {
		o.Status = "delivery_pending"
		o.PaymentIssue = "Payment confirmed. Sending your items now."
		o.PaidAt = time.Now().UTC()
	}
	a.store.mu.Lock()
	o.PaymentMessageID = a.store.data.Orders[o.ID].PaymentMessageID
	a.store.data.Orders[o.ID] = o
	saveErr := a.store.saveLocked()
	a.store.mu.Unlock()
	if saveErr != nil {
		return
	}
	if o.Status == "proof_invalid" {
		_ = a.tg.send(ctx, o.BuyerID, "❌ <b>This TxID was not accepted</b>\n\n"+esc(o.PaymentIssue)+"\n\nChecking this same transaction again will not fix it. Submit the correct TxID for this order, or contact support if you already paid. <b>Do not send another payment just to retry verification.</b>", paymentMarkup(o))
	}
	_ = a.sendPaymentCard(ctx, o.BuyerID, o, o.PaymentMessageID)
	if verified && err == nil {
		_ = a.deliverLocked(ctx, orderID)
	}
}

func (a *App) deliverOrder(ctx context.Context, orderID string) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	if !ok || (o.Status != "payment_submitted" && o.Status != "delivery_pending") {
		a.store.mu.Unlock()
		return errors.New("Order is not awaiting verification or delivery.")
	}
	if o.Status == "payment_submitted" {
		o.Status = "delivery_pending"
		o.PaidAt = time.Now().UTC()
		a.store.data.Orders[o.ID] = o
		if err := a.store.saveLocked(); err != nil {
			a.store.mu.Unlock()
			return err
		}
	}
	a.store.mu.Unlock()
	return a.deliverLocked(ctx, orderID)
}

func deliveryMessages(o Order, payloads []string) []string {
	header := fmt.Sprintf("✅ <b>Your order is ready</b>\n\nOrder: <code>%s</code>\nItems purchased: <b>%s × %d</b>\nPaid: <b>%.2f USDT</b>\n\n", esc(o.ID), esc(o.ProductName), o.Quantity, o.Amount)
	messages := []string{header}
	// Split before HTML escaping, at rune boundaries; large credentials remain readable.
	appendParts := func(title, body string) {
		r := []rune(body)
		for len(r) > 0 {
			n := 400
			if n > len(r) {
				n = len(r)
			}
			messages = append(messages, title+"\n<pre>"+esc(string(r[:n]))+"</pre>")
			r = r[n:]
		}
	}
	appendParts("📖 Delivery instructions", o.DeliveryInstruction)
	for i, p := range payloads {
		appendParts(fmt.Sprintf("🔐 Item %d of %d", i+1, len(payloads)), p)
	}
	return messages
}

func (a *App) deliverLocked(ctx context.Context, orderID string) error {
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	if !ok || o.Status != "delivery_pending" {
		a.store.mu.Unlock()
		return errors.New("No delivery is pending.")
	}
	p := a.store.data.Products[o.SKU]
	if o.ProductName == "" {
		o.ProductName = p.Name
	}
	if o.DeliveryInstruction == "" {
		o.DeliveryInstruction = p.DeliveryInstruction
	}
	payloads := []string{}
	for _, sid := range o.StockIDs {
		x, exists := a.store.data.Stock[sid]
		if !exists || x.OrderID != o.ID || x.Sold {
			a.store.mu.Unlock()
			return errors.New("Reserved inventory needs owner attention.")
		}
		payloads = append(payloads, x.Payload)
	}
	if len(payloads) != o.Quantity || o.Quantity == 0 {
		a.store.mu.Unlock()
		return errors.New("Reserved quantity does not match order.")
	}
	a.store.data.Orders[o.ID] = o
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		return err
	}
	messages := deliveryMessages(o, payloads)
	for i := o.DeliveryPartsSent; i < len(messages); i++ {
		if err := a.tg.send(ctx, o.BuyerID, messages[i], nil); err != nil {
			return err
		}
		o.DeliveryPartsSent = i + 1
		a.store.mu.Lock()
		o.PaymentMessageID = a.store.data.Orders[o.ID].PaymentMessageID
		a.store.data.Orders[o.ID] = o
		err := a.store.saveLocked()
		a.store.mu.Unlock()
		if err != nil {
			return err
		}
	}
	o.Status = "delivered"
	o.PaymentIssue = ""
	a.store.mu.Lock()
	o.PaymentMessageID = a.store.data.Orders[o.ID].PaymentMessageID
	for _, sid := range o.StockIDs {
		x := a.store.data.Stock[sid]
		x.Sold = true
		a.store.data.Stock[sid] = x
	}
	a.store.data.Orders[o.ID] = o
	err = a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		return err
	}
	_ = a.sendPaymentCard(ctx, o.BuyerID, o, o.PaymentMessageID)
	if a.cfg.Channel != "" {
		_ = a.tg.send(ctx, a.cfg.Channel, fmt.Sprintf("🛍 <b>Purchase completed</b>\n\n%s × %d\n✅ Delivered successfully", esc(o.ProductName), o.Quantity), nil)
	}
	_ = a.tg.send(ctx, a.cfg.OwnerID, "✅ Delivered order <code>"+o.ID+"</code> — "+esc(o.ProductName), nil)
	return nil
}

func (a *App) verifyLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		a.expireUnpaidOrders(ctx)
		a.store.mu.Lock()
		orders := []Order{}
		for _, o := range a.store.data.Orders {
			if proofAllowed(o) || o.Status == "delivery_pending" {
				orders = append(orders, o)
			}
		}
		a.store.mu.Unlock()
		sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt.Before(orders[j].CreatedAt) })
		for _, o := range orders {
			if ctx.Err() != nil {
				return
			}
			if o.Status == "payment_submitted" || o.Status == "delivery_pending" {
				a.verifyOrder(ctx, o.ID)
			} else if o.PaymentMessageID > 0 {
				_ = a.sendPaymentCard(ctx, o.BuyerID, o, o.PaymentMessageID)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (a *App) expireUnpaidOrders(ctx context.Context) {
	if !a.opMu.TryLock() {
		return
	}
	defer a.opMu.Unlock()
	a.store.mu.Lock()
	expired := []Order{}
	for id, o := range a.store.data.Orders {
		if (o.Status == "awaiting_payment" || o.Status == "proof_invalid") && time.Since(o.CreatedAt) >= 30*time.Minute {
			for _, sid := range o.StockIDs {
				x := a.store.data.Stock[sid]
				if !x.Sold && x.OrderID == o.ID {
					x.OrderID = ""
					a.store.data.Stock[sid] = x
				}
			}
			o.Status = "expired"
			a.store.data.Orders[id] = o
			expired = append(expired, o)
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
	for _, o := range expired {
		_ = a.sendPaymentCard(ctx, o.BuyerID, o, o.PaymentMessageID)
	}
}
