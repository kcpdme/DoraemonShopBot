package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func resellerFixture(t *testing.T) (*App, string, http.Handler) {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.data.Products["KEY"] = Product{SKU: "KEY", Name: "Test key", Description: "Access", PriceUSDT: 4.50, Active: true, DeliveryInstruction: "Redeem privately"}
	s.data.Products["HIDDEN"] = Product{SKU: "HIDDEN", Name: "Hidden", PriceUSDT: 1}
	s.data.Stock["one"] = StockItem{ID: "one", SKU: "KEY", Payload: "private-one", AddedAt: time.Unix(1, 0)}
	s.data.Stock["two"] = StockItem{ID: "two", SKU: "KEY", Payload: "private-two", AddedAt: time.Unix(2, 0)}
	s.data.Stock["reserved"] = StockItem{ID: "reserved", SKU: "KEY", Payload: "private-reserved", OrderID: "someone-else"}
	s.data.Stock["sold"] = StockItem{ID: "sold", SKU: "KEY", Payload: "private-sold", Sold: true}
	s.data.Wallets[walletKey(42)] = WalletAccount{BalanceCents: 2000}
	raw, key, err := newResellerKey(42)
	if err != nil {
		t.Fatal(err)
	}
	s.data.ResellerKeys[resellerKeyHash(raw)] = key
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	a := &App{store: s, cfg: Config{Token: "test-token", OwnerID: 1, MiniAppURL: "https://example.test/shop/"}, tg: walletTestTG()}
	h, err := a.miniAppHandler()
	if err != nil {
		t.Fatal(err)
	}
	return a, raw, h
}
func resellerRequest(h http.Handler, method, path, token, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func requireResellerStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("API response may be cached")
	}
}
func decodedResellerOrder(t *testing.T, w *httptest.ResponseRecorder) resellerOrderView {
	t.Helper()
	var result struct {
		Order resellerOrderView `json:"order"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Order
}

const resellerRoot = "/api/v1/reseller"

func TestResellerAuthenticationCatalogAndRoutes(t *testing.T) {
	a, token, h := resellerFixture(t)
	for _, path := range []string{"/prices", "/prices/KEY", "/balance", "/orders", "/orders/missing"} {
		for _, invalid := range []string{"", "invalid", "ztr_" + strings.Repeat("a", 43)} {
			requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+path, invalid, "", ""), 401)
		}
	}
	for _, base := range []string{resellerRoot, "/shop" + resellerRoot} {
		w := resellerRequest(h, "GET", base+"/prices", token, "", "")
		requireResellerStatus(t, w, 200)
		var result struct {
			Products []resellerPrice `json:"products"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Products) != 1 || result.Products[0].PriceUSDT != "4.50" || result.Products[0].Stock != 2 {
			t.Fatalf("wrong catalog: %s", w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), "Redeem") {
			t.Fatal("catalog exposed delivery")
		}
	}
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices/HIDDEN", token, "", ""), 404)
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices/KEY", token, "", ""), 200)
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/unknown", token, "", ""), 404)
	w := resellerRequest(h, "POST", resellerRoot+"/prices", token, "", "")
	requireResellerStatus(t, w, 405)
	if w.Header().Get("Allow") != "GET" {
		t.Fatal("missing Allow")
	}
	// Query-string credentials are never accepted.
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices?api_key="+token, "", "", ""), 401)
	b, err := os.ReadFile(a.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), token) {
		t.Fatal("plaintext API key persisted")
	}
}
func TestResellerPurchaseRetryRestartAndOwnership(t *testing.T) {
	a, token, h := resellerFixture(t)
	body := `{"sku":"KEY","quantity":2}`
	w := resellerRequest(h, "POST", resellerRoot+"/orders", token, body, "purchase-1")
	requireResellerStatus(t, w, 201)
	order := decodedResellerOrder(t, w)
	if order.Status != "delivered" || order.AmountUSDT != "9.00" || strings.Join(order.Items, ",") != "private-one,private-two" || order.DeliveryInstruction != "Redeem privately" {
		t.Fatalf("bad order: %+v", order)
	}
	if wallet := a.walletAccount(42); wallet.BalanceCents != 1100 || len(wallet.Entries) != 1 {
		t.Fatalf("incorrect debit: %+v", wallet)
	}
	if !a.store.data.Stock["one"].Sold || a.store.data.Stock["reserved"].Sold {
		t.Fatal("incorrect stock sold")
	}
	requireResellerStatus(t, resellerRequest(h, "POST", resellerRoot+"/orders", token, `{"sku":"KEY","quantity":1}`, "purchase-1"), 409)
	// Delivery and retry survive product/stock deletion, wallet exhaustion, and restart.
	delete(a.store.data.Products, "KEY")
	delete(a.store.data.Stock, "one")
	delete(a.store.data.Stock, "two")
	if err := a.store.save(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(a.store.path)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	w = resellerRequest(h, "POST", resellerRoot+"/orders", token, body, "purchase-1")
	requireResellerStatus(t, w, 200)
	if w.Header().Get("Idempotency-Replayed") != "true" || decodedResellerOrder(t, w).ID != order.ID {
		t.Fatal("retry did not reuse order")
	}
	w = resellerRequest(h, "GET", resellerRoot+"/orders/"+order.ID, token, "", "")
	requireResellerStatus(t, w, 200)
	if strings.Join(decodedResellerOrder(t, w).Items, ",") != "private-one,private-two" {
		t.Fatal("delivery lost on deletion/restart")
	}
	w = resellerRequest(h, "GET", resellerRoot+"/orders?limit=1&offset=0", token, "", "")
	requireResellerStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), order.ID) || strings.Contains(w.Body.String(), "private-one") {
		t.Fatal("list leaked payload or omitted order")
	}
	other, key, err := newResellerKey(99)
	if err != nil {
		t.Fatal(err)
	}
	a.store.data.ResellerKeys[resellerKeyHash(other)] = key
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/orders/"+order.ID, other, "", ""), 404)
	w = resellerRequest(h, "GET", resellerRoot+"/orders", other, "", "")
	requireResellerStatus(t, w, 200)
	if strings.Contains(w.Body.String(), order.ID) {
		t.Fatal("another customer can list order")
	}
	w = resellerRequest(h, "GET", resellerRoot+"/balance", token, "", "")
	requireResellerStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"balanceUsdt":"11.00"`) {
		t.Fatal(w.Body.String())
	}
}
func TestResellerRejectsInvalidOrdersWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name, body, key string
		status          int
	}{
		{"missing key", `{"sku":"KEY","quantity":1}`, "", 400},
		{"oversized key", `{"sku":"KEY","quantity":1}`, strings.Repeat("a", 129), 400},
		{"zero quantity", `{"sku":"KEY","quantity":0}`, "q", 400},
		{"negative quantity", `{"sku":"KEY","quantity":-1}`, "q", 400},
		{"large quantity", `{"sku":"KEY","quantity":51}`, "q", 400},
		{"fraction", `{"sku":"KEY","quantity":1.5}`, "q", 400},
		{"sku alias", `{"sku":"K_EY","quantity":1}`, "q", 400},
		{"unknown field", `{"sku":"KEY","quantity":1,"buyerId":99}`, "q", 400},
		{"trailing JSON", `{"sku":"KEY","quantity":1} {}`, "q", 400},
		{"null", "null", "q", 400},
		{"large body", strings.Repeat(" ", 4096) + `{"sku":"KEY","quantity":1}`, "q", 400},
		{"stock", `{"sku":"KEY","quantity":3}`, "q", 409},
		{"hidden", `{"sku":"HIDDEN","quantity":1}`, "q", 404},
		{"missing product", `{"sku":"MISSING","quantity":1}`, "q", 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, token, h := resellerFixture(t)
			before, _ := json.Marshal(a.store.data)
			requireResellerStatus(t, resellerRequest(h, "POST", resellerRoot+"/orders", token, test.body, test.key), test.status)
			after, _ := json.Marshal(a.store.data)
			if string(before) != string(after) {
				t.Fatal("rejected order mutated state")
			}
		})
	}
	a, token, h := resellerFixture(t)
	a.store.data.Wallets[walletKey(42)] = WalletAccount{BalanceCents: 449}
	w := resellerRequest(h, "POST", resellerRoot+"/orders", token, `{"sku":"KEY","quantity":1}`, "poor")
	requireResellerStatus(t, w, 409)
	if !strings.Contains(w.Body.String(), "insufficient_balance") || len(a.store.data.Orders) != 0 || a.store.data.Stock["one"].Sold {
		t.Fatal("failed purchase altered state")
	}
	for _, query := range []string{"limit=0", "limit=101", "offset=-1", "offset=no", "limit=1&limit=2"} {
		requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/orders?"+query, token, "", ""), 400)
	}
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/orders?offset=9223372036854775807", token, "", ""), 200)
}
func TestResellerSaveFailureRollsBackAndCanRetry(t *testing.T) {
	a, token, h := resellerFixture(t)
	original := a.store.path
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	a.store.path = filepath.Join(blocker, "store.json")
	w := resellerRequest(h, "POST", resellerRoot+"/orders", token, `{"sku":"KEY","quantity":1}`, "retry-save")
	requireResellerStatus(t, w, 500)
	if strings.Contains(w.Body.String(), blocker) {
		t.Fatal("storage path exposed")
	}
	if a.walletAccount(42).BalanceCents != 2000 || len(a.store.data.Orders) != 0 || a.store.data.Stock["one"].Sold {
		t.Fatal("failed save retained debit or stock sale")
	}
	a.store.path = original
	requireResellerStatus(t, resellerRequest(h, "POST", resellerRoot+"/orders", token, `{"sku":"KEY","quantity":1}`, "retry-save"), 201)
}
func TestResellerConcurrentRetriesAndLastStock(t *testing.T) {
	for _, sameKey := range []bool{true, false} {
		t.Run(fmt.Sprint(sameKey), func(t *testing.T) {
			a, token, h := resellerFixture(t)
			delete(a.store.data.Stock, "two")
			if err := a.store.save(); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			codes := make(chan int, 20)
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					key := "same"
					if !sameKey {
						key = fmt.Sprint(i)
					}
					w := resellerRequest(h, "POST", resellerRoot+"/orders", token, `{"sku":"KEY","quantity":1}`, key)
					codes <- w.Code
				}(i)
			}
			wg.Wait()
			close(codes)
			created := 0
			for code := range codes {
				if code == 201 {
					created++
				} else if sameKey && code != 200 || !sameKey && code != 409 {
					t.Fatalf("unexpected status %d", code)
				}
			}
			if created != 1 || len(a.store.data.Orders) != 1 || a.walletAccount(42).BalanceCents != 1550 || len(a.walletAccount(42).Entries) != 1 {
				t.Fatal("concurrent requests double charged or oversold")
			}
		})
	}
}
func TestResellerRateLimitAndMembership(t *testing.T) {
	t.Run("limit", func(t *testing.T) {
		_, token, h := resellerFixture(t)
		for i := 0; i < 60; i++ {
			requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/balance", token, "", ""), 200)
		}
		w := resellerRequest(h, "GET", resellerRoot+"/prices", token, "", "")
		requireResellerStatus(t, w, 429)
		if w.Header().Get("Retry-After") == "" {
			t.Fatal("missing retry interval")
		}
		limiter := resellerLimiter{}
		now := time.Now()
		for i := 0; i < 60; i++ {
			limiter.allow(42, now)
		}
		if ok, _ := limiter.allow(42, now.Add(time.Minute)); !ok {
			t.Fatal("rate window did not reset")
		}
	})
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			a, token, h := resellerFixture(t)
			a.cfg.Channel = "@test"
			a.cfg.RequireChannel = true
			a.tg = &TG{client: &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
				if failure {
					return nil, errors.New("offline")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"status":"left"}}`)), Header: http.Header{}}, nil
			})}}
			status := 403
			if failure {
				status = 503
			}
			requireResellerStatus(t, resellerRequest(h, "POST", resellerRoot+"/orders", token, `{"sku":"KEY","quantity":1}`, "gate"), status)
			if len(a.store.data.Orders) != 0 || a.walletAccount(42).BalanceCents != 2000 {
				t.Fatal("membership failure charged customer")
			}
		})
	}
}

func TestResellerBotKeyLifecycleAndPrivateChat(t *testing.T) {
	a, old, h := resellerFixture(t)
	var messages []string
	failSend := false
	a.tg = &TG{client: &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if failSend {
			return nil, errors.New("Telegram unavailable")
		}
		messages = append(messages, payload.Text)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`)), Header: http.Header{}}, nil
	})}}
	ctx := context.Background()
	a.handleMessage(ctx, &Message{Chat: Chat{ID: -100}, From: &User{ID: 99}, Text: "/apikey generate"})
	if len(messages) != 0 || len(a.store.data.ResellerKeys) != 1 {
		t.Fatal("group chat created a key")
	}
	a.handleMessage(ctx, &Message{Chat: Chat{ID: 99}, From: &User{ID: 99}, Text: "/apikey generate"})
	tokenPattern := regexp.MustCompile(`ztr_[A-Za-z0-9_-]{43}`)
	newToken := tokenPattern.FindString(strings.Join(messages, "\n"))
	if newToken == "" {
		t.Fatal("private creation did not return key")
	}
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices", newToken, "", ""), 200)
	// Duplicate generation cannot replace a live key or reveal it again.
	messages = nil
	a.resellerKeyCommand(ctx, 99, "generate")
	if tokenPattern.MatchString(strings.Join(messages, "\n")) {
		t.Fatal("active key displayed again")
	}
	a.resellerKeyCommand(ctx, 42, "rotate")
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices", old, "", ""), 200)
	confirm := "confirmrotate:" + resellerKeyVersion(resellerKeyHash(old))
	failSend = true
	a.resellerKeyCommand(ctx, 42, confirm)
	failSend = false
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices", old, "", ""), 200)
	messages = nil
	a.handleCallback(ctx, &Callback{From: User{ID: 42}, Message: &Message{Chat: Chat{ID: 42}}, Data: "apikey:" + confirm})
	rotated := tokenPattern.FindString(strings.Join(messages, "\n"))
	if rotated == "" {
		t.Fatal("rotation did not deliver key")
	}
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices", old, "", ""), 401)
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices", rotated, "", ""), 200)
	messages = nil
	a.resellerKeyCommand(ctx, 42, confirm)
	if tokenPattern.MatchString(strings.Join(messages, "\n")) {
		t.Fatal("stale confirmation rotated key twice")
	}
	// A key rotation preserves customer-level order idempotency.
	requireResellerStatus(t, resellerRequest(h, "POST", resellerRoot+"/orders", rotated, `{"sku":"KEY","quantity":1}`, "lifecycle"), 201)
	a.resellerKeyCommand(ctx, 42, "confirmrevoke:"+resellerKeyVersion(resellerKeyHash(rotated)))
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices", rotated, "", ""), 401)
	messages = nil
	a.resellerKeyCommand(ctx, 42, "generate")
	replacement := tokenPattern.FindString(strings.Join(messages, "\n"))
	requireResellerStatus(t, resellerRequest(h, "POST", resellerRoot+"/orders", replacement, `{"sku":"KEY","quantity":1}`, "lifecycle"), 200)
	if len(a.walletAccount(42).Entries) != 1 {
		t.Fatal("key replacement bypassed idempotency")
	}
}

func TestResellerKeySaveFailurePreservesPreviousKey(t *testing.T) {
	a, old, h := resellerFixture(t)
	var messages []string
	a.tg = &TG{client: &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		messages = append(messages, payload.Text)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`)), Header: http.Header{}}, nil
	})}}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	a.store.path = filepath.Join(blocker, "store.json")
	a.resellerKeyCommand(context.Background(), 42, "confirmrotate:"+resellerKeyVersion(resellerKeyHash(old)))
	attempted := regexp.MustCompile(`ztr_[A-Za-z0-9_-]{43}`).FindString(strings.Join(messages, "\n"))
	if attempted == "" || !strings.Contains(strings.Join(messages, "\n"), "Key activation failed") {
		t.Fatal("rotation failure not reported")
	}
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices", attempted, "", ""), 401)
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices", old, "", ""), 200)
	a.resellerKeyCommand(context.Background(), 42, "confirmrevoke:"+resellerKeyVersion(resellerKeyHash(old)))
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/prices", old, "", ""), 200)
}

func TestResellerRevokedDuringMembershipCheckCannotSpend(t *testing.T) {
	a, token, h := resellerFixture(t)
	a.cfg.Channel = "@test"
	a.cfg.RequireChannel = true
	a.tg = &TG{client: &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
		a.store.mu.Lock()
		delete(a.store.data.ResellerKeys, resellerKeyHash(token))
		err := a.store.saveLocked()
		a.store.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"status":"member"}}`)), Header: http.Header{}}, nil
	})}}
	requireResellerStatus(t, resellerRequest(h, "POST", resellerRoot+"/orders", token, `{"sku":"KEY","quantity":1}`, "revoked"), 401)
	if a.walletAccount(42).BalanceCents != 2000 || len(a.store.data.Orders) != 0 {
		t.Fatal("revoked key spent wallet funds")
	}
}

func TestResellerLegacyStoreAndAuthBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(path, []byte(`{"Products":{},"Stock":{},"Orders":{},"Started":{"42":"Buyer"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(path)
	if err != nil || s.data.ResellerKeys == nil {
		t.Fatalf("legacy store failed: %v", err)
	}
	a, token, h := resellerFixture(t)
	// A reseller key must not grant owner or Mini App privileges.
	for _, route := range []string{"/api/mini-app/catalog", "/api/mini-app/admin/products"} {
		w := resellerRequest(h, "GET", route, token, "", "")
		if w.Code != 401 {
			t.Fatalf("reseller token crossed auth boundary: %d", w.Code)
		}
	}
	a.store.data.Orders["chat-order"] = Order{ID: "chat-order", BuyerID: 42, Status: "awaiting_payment"}
	requireResellerStatus(t, resellerRequest(h, "GET", resellerRoot+"/orders/chat-order", token, "", ""), 404)
	w := resellerRequest(h, "GET", resellerRoot+"/orders", token, "", "")
	requireResellerStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "chat-order") {
		t.Fatal("reseller history mixed in chat orders")
	}
}
