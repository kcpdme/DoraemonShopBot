package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func walletTestTG() *TG {
	return &TG{client: &http.Client{Transport: paymentTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":42}}`)), Header: http.Header{}}, nil
	})}}
}

func TestParseWalletTopUpAmounts(t *testing.T) {
	for input, want := range map[string]int64{"1": 100, "12.5": 1250, "$99.99": 9999} {
		got, err := parseUSDTAmount(input)
		if err != nil || got != want {
			t.Fatalf("parse %q: got=%d err=%v", input, got, err)
		}
	}
	for _, input := range []string{"0.99", "1.001", "-4", "ten", "100001"} {
		if _, err := parseUSDTAmount(input); err == nil {
			t.Fatalf("accepted invalid top-up amount %q", input)
		}
	}
}

func TestVerifiedWalletDepositCreditsOnce(t *testing.T) {
	a, payment := paymentFixture(t, "bep20", nil)
	s, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	a.store = s
	base := a.tg.client.Transport
	a.tg.client.Transport = paymentTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.telegram.org" {
			return walletTestTG().client.Transport.RoundTrip(r)
		}
		return base.RoundTrip(r)
	})
	d := WalletDeposit{ID: "topup", BuyerID: 77, Network: payment.Network, AmountCents: 1234, TxHash: payment.TxHash, Status: "payment_submitted", CreatedAt: payment.CreatedAt}
	s.data.WalletDeposits[d.ID] = d
	a.verifyWalletDeposit(context.Background(), d.ID)
	got := s.data.WalletDeposits[d.ID]
	account := s.data.Wallets[walletKey(d.BuyerID)]
	if got.Status != "credited" || account.BalanceCents != 1234 || len(account.Entries) != 1 || account.Entries[0].AmountCents != 1234 {
		t.Fatalf("wallet top-up was not credited correctly: deposit=%+v wallet=%+v", got, account)
	}
	a.verifyWalletDeposit(context.Background(), d.ID)
	if account = s.data.Wallets[walletKey(d.BuyerID)]; account.BalanceCents != 1234 || len(account.Entries) != 1 {
		t.Fatalf("wallet top-up was credited twice: %+v", account)
	}
}

func TestWalletPurchaseDebitsThenDelivers(t *testing.T) {
	s, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	product := Product{SKU: "ITEM", Name: "Private item", PriceUSDT: 4.50, DeliveryInstruction: "Redeem it", Active: true}
	s.data.Products[product.SKU] = product
	s.data.Stock["stock"] = StockItem{ID: "stock", SKU: product.SKU, Payload: "secret-code"}
	s.data.Wallets[walletKey(88)] = WalletAccount{BalanceCents: 500}
	a := &App{store: s, cfg: Config{OwnerID: 1}, tg: walletTestTG()}
	o, err := a.purchaseWithWallet(context.Background(), &User{ID: 88}, product.SKU, 1)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != "delivery_pending" && o.Status != "delivered" {
		t.Fatalf("wallet purchase not ready for delivery: %+v", o)
	}
	stored := s.data.Orders[o.ID]
	account := s.data.Wallets[walletKey(88)]
	if stored.Status != "delivered" || account.BalanceCents != 50 || len(account.Entries) != 1 || account.Entries[0].AmountCents != -450 || !s.data.Stock["stock"].Sold {
		t.Fatalf("wallet purchase did not debit and deliver atomically: order=%+v wallet=%+v stock=%+v", stored, account, s.data.Stock["stock"])
	}
	if !stored.PaidAt.Before(time.Now().Add(time.Second)) {
		t.Fatal("wallet purchase did not record payment time")
	}
}

func TestSaleAnnouncementNeverIncludesBuyerIdentity(t *testing.T) {
	o := Order{BuyerID: 999, BuyerName: "@privatebuyer", ID: "ord_private", ProductName: "Gemini access", Quantity: 2, Amount: 9.50}
	text := saleAnnouncement(o)
	for _, forbidden := range []string{"privatebuyer", "999", "ord_private"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sale announcement exposed buyer data %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "Gemini access") || !strings.Contains(text, "$9.50 USDT") {
		t.Fatalf("sale announcement omitted public sale details: %s", text)
	}
}

func TestTransactionHashCannotBeSharedByOrderAndWalletTopUp(t *testing.T) {
	s, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	hash := "0x" + strings.Repeat("a", 64)
	s.data.Orders["order"] = Order{ID: "order", Network: "bep20", TxHash: hash, Status: "proof_invalid"}
	a := &App{store: s}
	s.mu.Lock()
	inUse := a.txHashInUseLocked("bep20", hash, "topup")
	s.mu.Unlock()
	if !inUse {
		t.Fatal("a transaction hash already submitted for an order was accepted for a wallet top-up")
	}
}
