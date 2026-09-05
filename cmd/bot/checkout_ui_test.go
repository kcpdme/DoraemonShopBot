package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func checkoutUIFixture(t *testing.T, stock int) *App {
	t.Helper()
	s, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	s.data.Products["DIGITAL"] = Product{SKU: "DIGITAL", Name: "Digital <item>", Description: "A & B", PriceUSDT: 2.25, Active: true}
	for i := 0; i < stock; i++ {
		key := fmt.Sprintf("stock%d", i)
		s.data.Stock[key] = StockItem{ID: key, SKU: "DIGITAL"}
	}
	return &App{store: s, cfg: Config{BEP20Address: "0xBscRecipient", PolygonAddress: "0xPolygonRecipient"}}
}

func checkoutButtonData(kb *Markup) []string {
	var data []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			data = append(data, b.Data)
		}
	}
	return data
}

func TestCheckoutQuantityRespectsAvailableStockAndShowsTotal(t *testing.T) {
	a := checkoutUIFixture(t, 3)
	// Reserved and sold stock must not become purchasable quantity buttons.
	a.store.data.Stock["reserved"] = StockItem{SKU: "DIGITAL", OrderID: "another-order"}
	a.store.data.Stock["sold"] = StockItem{SKU: "DIGITAL", Sold: true}
	text, kb := a.quantityText("DIGITAL")
	if !strings.Contains(text, "Digital &lt;item&gt;") || !strings.Contains(text, "<b>3</b>") {
		t.Fatalf("quantity screen lost escaping or available stock: %s", text)
	}
	for _, data := range checkoutButtonData(kb) {
		if data == "qty:DIGITAL:5" || data == "qty:DIGITAL:10" {
			t.Fatalf("quantity button exceeds stock: %s", data)
		}
	}
	text, kb, err := a.networkChoice("DIGITAL", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "6.75 USDT") {
		t.Fatalf("network selection omitted quantity total: %s", text)
	}
	want := map[string]bool{"network:bep20:DIGITAL:3": false, "network:polygon:DIGITAL:3": false}
	for _, data := range checkoutButtonData(kb) {
		if _, ok := want[data]; ok {
			want[data] = true
		}
	}
	for data, found := range want {
		if !found {
			t.Errorf("missing quantity-preserving network callback %s", data)
		}
	}
	if _, _, err := a.networkChoice("DIGITAL", 4); err == nil {
		t.Fatal("network picker accepted quantity above available stock")
	}
}

func TestSoldOutProductHasNoBuyButton(t *testing.T) {
	a := checkoutUIFixture(t, 0)
	text, kb := a.productText("DIGITAL")
	if !strings.Contains(text, "sold out") {
		t.Fatalf("missing sold-out explanation: %s", text)
	}
	for _, data := range checkoutButtonData(kb) {
		if strings.HasPrefix(data, "buy:") {
			t.Fatal("sold-out product offers checkout")
		}
	}
}

func TestPaymentCardUsesSnapshotAndSelectedWallet(t *testing.T) {
	a := checkoutUIFixture(t, 3)
	o := Order{ID: "order", SKU: "DIGITAL", ProductName: "Purchased <name>", Quantity: 3, Amount: 6.75, Network: "polygon", Status: "awaiting_payment", CreatedAt: time.Now()}
	text := a.paymentText(o)
	for _, want := range []string{"Purchased &lt;name&gt; × 3", "6.75 USDT", "0xPolygonRecipient", "Polygon PoS", "Submit TxID", "Time left:"} {
		if !strings.Contains(text, want) {
			t.Errorf("payment card missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "0xBscRecipient") || strings.Contains(text, "checks automatically") {
		t.Fatalf("card has wrong wallet or promises verification before proof submission: %s", text)
	}
}

func TestInvalidProofShowsActionAndStopsAutomaticRetryPromise(t *testing.T) {
	a := checkoutUIFixture(t, 3)
	o := Order{ID: "order", SKU: "DIGITAL", Status: "proof_invalid", PaymentIssue: "This <old> transaction predates your order.", CreatedAt: time.Now()}
	text := a.paymentText(o)
	if !strings.Contains(text, "Automatic checks have stopped") || !strings.Contains(text, "&lt;old&gt;") || strings.Contains(text, "every 30 seconds") {
		t.Fatalf("invalid-proof card must explain final failure safely: %s", text)
	}
	data := strings.Join(checkoutButtonData(paymentMarkup(o)), " ")
	if !strings.Contains(data, "proof:order") || strings.Contains(data, "check:order") {
		t.Fatalf("invalid-proof actions should request replacement, not retry invalid payment: %s", data)
	}
}

func TestClosedPaymentCardsNeverAskForNewPayment(t *testing.T) {
	a := checkoutUIFixture(t, 1)
	for _, status := range []string{"delivered", "delivery_pending", "cancelled", "expired", "payment_rejected"} {
		t.Run(status, func(t *testing.T) {
			o := Order{ID: "order", SKU: "DIGITAL", Status: status, CreatedAt: time.Now()}
			text := a.paymentText(o)
			if strings.Contains(text, "Send exactly") || strings.Contains(text, "0xBscRecipient") {
				t.Fatalf("closed checkout displays payment instructions: %s", text)
			}
			for _, data := range checkoutButtonData(paymentMarkup(o)) {
				if strings.HasPrefix(data, "proof:") || strings.HasPrefix(data, "check:") || strings.HasPrefix(data, "cancel:") {
					t.Fatalf("closed checkout exposes action %s", data)
				}
			}
		})
	}
	o := Order{ID: "old", SKU: "DIGITAL", Status: "awaiting_payment", CreatedAt: time.Now().Add(-31 * time.Minute)}
	text := a.paymentText(o)
	if !strings.Contains(text, "Payment window ended") || strings.Contains(text, "0xBscRecipient") {
		t.Fatalf("expired reservation awaiting worker should not request payment: %s", text)
	}
	for _, data := range checkoutButtonData(paymentMarkup(o)) {
		if strings.HasPrefix(data, "proof:") {
			t.Fatal("expired reservation still offers payment submission")
		}
	}
}

func TestPendingPaymentCardExplainsCurrentIssue(t *testing.T) {
	a := checkoutUIFixture(t, 1)
	o := Order{ID: "order", SKU: "DIGITAL", Status: "payment_submitted", PaymentIssue: "Waiting for confirmations: 1 < 3", CreatedAt: time.Now()}
	text := a.paymentText(o)
	if !strings.Contains(text, "Waiting for confirmations: 1 &lt; 3") {
		t.Fatalf("pending card does not show escaped current issue: %s", text)
	}
}

func TestNavigatingAwayDetachesCardAndSkipsCapturedCountdown(t *testing.T) {
	a := checkoutUIFixture(t, 2)
	o := Order{ID: "order", SKU: "DIGITAL", BuyerID: 42, Status: "awaiting_payment", CreatedAt: time.Now(), PaymentMessageID: 10}
	a.store.data.Orders[o.ID] = o
	other := o
	other.ID, other.BuyerID = "other-customer", 99
	a.store.data.Orders[other.ID] = other
	requests := 0
	a.tg = &TG{token: "test", client: &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
	})}}
	c := &Callback{Data: "catalog", From: User{ID: 42}, Message: &Message{MessageID: 10, Chat: Chat{ID: 42}}}
	if !a.handleCheckoutCallback(context.Background(), c, 42) {
		t.Fatal("catalog callback was not handled")
	}
	if a.store.data.Orders[o.ID].PaymentMessageID != 0 {
		t.Fatal("navigated payment message is still attached to countdown")
	}
	if a.store.data.Orders[other.ID].PaymentMessageID != 10 {
		t.Fatal("navigation detached another customer's same-numbered message")
	}
	if err := a.sendPaymentCard(context.Background(), 42, o, 10); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("captured countdown overwrote new catalog screen; %d API requests", requests)
	}
}

func TestPaymentCardRefreshUsesLatestOrderAndDoesNotDuplicateOnTransientFailure(t *testing.T) {
	for _, description := range []string{"Too Many Requests: retry after 10", "Internal Server Error", "Bad Request: message can't be edited"} {
		t.Run(description, func(t *testing.T) {
			a := checkoutUIFixture(t, 1)
			old := Order{ID: "order", SKU: "DIGITAL", BuyerID: 42, Status: "awaiting_payment", CreatedAt: time.Now(), PaymentMessageID: 10}
			latest := old
			latest.Status = "delivered"
			a.store.data.Orders[old.ID] = latest
			requests := 0
			a.tg = &TG{token: "test", client: &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if text, _ := body["text"].(string); !strings.Contains(text, "Delivered") || strings.Contains(text, "Send exactly") {
					t.Errorf("refresh rendered stale unpaid order: %s", text)
				}
				reply := `{"ok":true,"result":{"message_id":11}}`
				if strings.HasSuffix(r.URL.Path, "editMessageText") {
					encoded, _ := json.Marshal(map[string]any{"ok": false, "description": description})
					reply = string(encoded)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)), Header: make(http.Header)}, nil
			})}}
			err := a.sendPaymentCard(context.Background(), 42, old, 10)
			if strings.Contains(description, "can't be edited") {
				if err != nil || requests != 2 || a.store.data.Orders[old.ID].PaymentMessageID != 11 {
					t.Fatalf("uneditable message did not get replacement card: err=%v requests=%d", err, requests)
				}
			} else if err == nil || requests != 1 {
				t.Fatalf("transient edit failure should return error without sending a duplicate: err=%v requests=%d", err, requests)
			}
		})
	}
}
