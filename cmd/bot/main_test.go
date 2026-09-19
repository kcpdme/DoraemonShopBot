package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCreateOrderReservesOnlyAvailableStock(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.data.Products["KEY"] = Product{SKU: "KEY", Name: "Key", PriceUSDT: 5, Active: true, CreatedAt: time.Now()}
	s.data.Stock["one"] = StockItem{ID: "one", SKU: "KEY", Payload: "secret"}
	a := &App{store: s}
	o, err := a.createOrder(&User{ID: 42, FirstName: "Buyer"}, "KEY", "polygon")
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != "awaiting_payment" || len(o.StockIDs) != 1 {
		t.Fatalf("bad order: %#v", o)
	}
	if got := s.data.Stock["one"].OrderID; got != o.ID {
		t.Fatalf("stock not reserved: %q", got)
	}
	if _, err := a.createOrder(&User{ID: 99}, "KEY", "bep20"); err == nil {
		t.Fatal("second order should not be able to reserve the same stock")
	}
}

func TestSKU(t *testing.T) {
	if got := sku(" pro key_2026 "); got != "PROKEY2026" {
		t.Fatalf("got %q", got)
	}
}

func TestCatalogStockIndicators(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, product := range []Product{
		{SKU: "GREEN", Name: "Green stock", PriceUSDT: 1, Active: true, CreatedAt: time.Now()},
		{SKU: "BLUE", Name: "Blue stock", PriceUSDT: 2, Active: true, CreatedAt: time.Now().Add(-time.Second)},
		{SKU: "RED", Name: "Red stock", PriceUSDT: 3, Active: true, CreatedAt: time.Now().Add(-2 * time.Second)},
	} {
		store.data.Products[product.SKU] = product
	}
	for i := 0; i < 6; i++ {
		store.data.Stock["green-"+string(rune('a'+i))] = StockItem{SKU: "GREEN"}
	}
	for i := 0; i < 5; i++ {
		store.data.Stock["blue-"+string(rune('a'+i))] = StockItem{SKU: "BLUE"}
	}

	_, keyboard := (&App{store: store}).productsText()
	if keyboard == nil {
		t.Fatal("catalog has no buttons")
	}
	buttons := map[string]Button{}
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			buttons[button.Data] = button
		}
	}
	for data, want := range map[string]struct{ text, style string }{
		"product:GREEN": {"🛒 Green stock · $1.00 · 6 available", "success"},
		"product:BLUE":  {"🛒 Blue stock · $2.00 · 5 available", "primary"},
		"product:RED":   {"🛒 Red stock · $3.00 · Sold out", "danger"},
	} {
		button, ok := buttons[data]
		if !ok || button.Text != want.text || button.Style != want.style {
			t.Errorf("catalog button %s = %#v, want text=%q style=%q", data, button, want.text, want.style)
		}
	}
}

func TestExactUSDTAmount(t *testing.T) {
	if !exactUSDTAmount(12.34, "12340000", "6") {
		t.Fatal("six-decimal USDT amount should match")
	}
	if !exactUSDTAmount(12.34, "12340000000000000000", "18") {
		t.Fatal("eighteen-decimal USDT amount should match")
	}
	if exactUSDTAmount(12.34, "12350000", "6") {
		t.Fatal("wrong amount must not match")
	}
}

func TestNormalizeHash(t *testing.T) {
	hash := "0x1111111111111111111111111111111111111111111111111111111111111111"
	if got := normalizeHash("https://bscscan.com/tx/" + hash + "?foo=bar"); got != hash {
		t.Fatalf("got %q", got)
	}
}
