package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Keys are indexed by SHA-256 digest. The plaintext bearer token is never saved.
type ResellerKey struct {
	BuyerID   int64
	Hint      string
	CreatedAt time.Time
}

// This snapshot survives stock/product deletion and makes retries durable.
// It is committed together with the debit and stock sale in one store write.
type ResellerOrderData struct {
	IdempotencyKey string
	AmountCents    int64
	Payloads       []string
}

type resellerRateWindow struct {
	Start time.Time
	Count int
}
type resellerLimiter struct {
	mu      sync.Mutex
	windows map[int64]resellerRateWindow
}

func (l *resellerLimiter) allow(buyer int64, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = make(map[int64]resellerRateWindow)
	}
	for id, window := range l.windows {
		if now.Sub(window.Start) >= time.Minute {
			delete(l.windows, id)
		}
	}
	window := l.windows[buyer]
	if window.Start.IsZero() {
		window.Start = now
	}
	if window.Count >= 60 {
		return false, max(1, int(window.Start.Add(time.Minute).Sub(now).Seconds())+1)
	}
	window.Count++
	l.windows[buyer] = window
	return true, 0
}
func resellerKeyHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
func newResellerKey(buyer int64) (string, ResellerKey, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", ResellerKey{}, err
	}
	raw := "ztr_" + base64.RawURLEncoding.EncodeToString(b)
	return raw, ResellerKey{BuyerID: buyer, Hint: raw[len(raw)-4:], CreatedAt: time.Now().UTC()}, nil
}
func (a *App) resellerAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Authorization")
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(fields[1]) != 47 || !strings.HasPrefix(fields[1], "ztr_") {
		resellerUnauthorized(w)
		return
	}
	hash := resellerKeyHash(fields[1])
	a.store.mu.Lock()
	key, ok := a.store.data.ResellerKeys[hash]
	a.store.mu.Unlock()
	if !ok || key.BuyerID <= 0 {
		resellerUnauthorized(w)
		return
	}
	if allowed, retry := a.resellerLimits.allow(key.BuyerID, time.Now()); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		resellerError(w, 429, "rate_limited", "Limit is 60 requests per minute per customer.")
		return
	}
	route := strings.TrimPrefix(r.URL.Path, "/api/v1/reseller/")
	switch {
	case route == "prices" || strings.HasPrefix(route, "prices/"):
		if !resellerMethod(w, r, http.MethodGet) {
			return
		}
		a.resellerPrices(w, strings.TrimPrefix(route, "prices"))
	case route == "balance":
		if !resellerMethod(w, r, http.MethodGet) {
			return
		}
		account := a.walletAccount(key.BuyerID)
		writeJSON(w, 200, map[string]any{"balanceUsdt": formatUSDT(account.BalanceCents), "currency": "USDT"})
	case route == "orders":
		if r.Method == http.MethodGet {
			a.resellerOrders(w, r, key.BuyerID, "")
			return
		}
		if !resellerMethod(w, r, http.MethodPost, http.MethodGet) {
			return
		}
		a.resellerPlaceOrder(w, r, hash, key.BuyerID)
	case strings.HasPrefix(route, "orders/"):
		if !resellerMethod(w, r, http.MethodGet) {
			return
		}
		a.resellerOrders(w, r, key.BuyerID, strings.TrimPrefix(route, "orders/"))
	default:
		resellerError(w, 404, "not_found", "Endpoint not found.")
	}
}
func resellerUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	resellerError(w, 401, "invalid_api_key", "Provide an active API key in Authorization: Bearer.")
}
func resellerError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}
func resellerMethod(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, method := range allowed {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	resellerError(w, 405, "method_not_allowed", "Method not allowed.")
	return false
}

type resellerPrice struct {
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceUSDT   string `json:"priceUsdt"`
	Currency    string `json:"currency"`
	Stock       int    `json:"stock"`
}

func (a *App) resellerPrices(w http.ResponseWriter, suffix string) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	products := []resellerPrice{}
	for _, p := range a.store.data.Products {
		cents := math.Round(p.PriceUSDT * 100)
		if !p.Active || math.IsNaN(cents) || math.IsInf(cents, 0) || cents < 1 || cents > 1e9 {
			continue
		}
		stock := 0
		for _, item := range a.store.data.Stock {
			if item.SKU == p.SKU && !item.Sold && item.OrderID == "" {
				stock++
			}
		}
		product := resellerPrice{SKU: p.SKU, Name: p.Name, Description: p.Description, PriceUSDT: formatUSDT(int64(cents)), Currency: "USDT", Stock: stock}
		if suffix == "/"+p.SKU {
			writeJSON(w, 200, map[string]any{"product": product})
			return
		}
		products = append(products, product)
	}
	if suffix != "" {
		resellerError(w, 404, "product_not_found", "Product not found.")
		return
	}
	sort.Slice(products, func(i, j int) bool { return products[i].SKU < products[j].SKU })
	writeJSON(w, 200, map[string]any{"products": products})
}

type resellerOrderView struct {
	ID                  string    `json:"id"`
	SKU                 string    `json:"sku"`
	Name                string    `json:"name"`
	Quantity            int       `json:"quantity"`
	AmountUSDT          string    `json:"amountUsdt"`
	Currency            string    `json:"currency"`
	Status              string    `json:"status"`
	CreatedAt           time.Time `json:"createdAt"`
	PaidAt              time.Time `json:"paidAt"`
	DeliveryInstruction string    `json:"deliveryInstruction,omitempty"`
	Items               []string  `json:"items,omitempty"`
}

func resellerOrderDTO(o Order, detail bool) resellerOrderView {
	result := resellerOrderView{ID: o.ID, SKU: o.SKU, Name: o.ProductName, Quantity: o.Quantity, AmountUSDT: formatUSDT(o.Reseller.AmountCents), Currency: "USDT", Status: o.Status, CreatedAt: o.CreatedAt, PaidAt: o.PaidAt}
	if detail && o.Status == "delivered" {
		result.DeliveryInstruction = o.DeliveryInstruction
		result.Items = o.Reseller.Payloads
	}
	return result
}
func (a *App) resellerOrders(w http.ResponseWriter, r *http.Request, buyer int64, orderID string) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if orderID != "" {
		o, ok := a.store.data.Orders[orderID]
		if !ok || o.BuyerID != buyer || o.Reseller == nil {
			resellerError(w, 404, "order_not_found", "Order not found.")
			return
		}
		writeJSON(w, 200, map[string]any{"order": resellerOrderDTO(o, true)})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/") {
		resellerError(w, 404, "order_not_found", "Order not found.")
		return
	}
	limit, offset := 50, 0
	for name, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		if raw, exists := r.URL.Query()[name]; exists {
			if len(raw) != 1 {
				resellerError(w, 400, "invalid_pagination", "Use limit 1–100 and a non-negative offset.")
				return
			}
			n, err := strconv.Atoi(raw[0])
			if err != nil {
				resellerError(w, 400, "invalid_pagination", "Use limit 1–100 and a non-negative offset.")
				return
			}
			*target = n
		}
	}
	if limit < 1 || limit > 100 || offset < 0 {
		resellerError(w, 400, "invalid_pagination", "Use limit 1–100 and a non-negative offset.")
		return
	}
	orders := []Order{}
	for _, o := range a.store.data.Orders {
		if o.BuyerID == buyer && o.Reseller != nil {
			orders = append(orders, o)
		}
	}
	sort.Slice(orders, func(i, j int) bool {
		if orders[i].CreatedAt.Equal(orders[j].CreatedAt) {
			return orders[i].ID < orders[j].ID
		}
		return orders[i].CreatedAt.After(orders[j].CreatedAt)
	})
	total := len(orders)
	start := min(offset, total)
	end := start + min(limit, total-start)
	result := []resellerOrderView{}
	for _, o := range orders[start:end] {
		result = append(result, resellerOrderDTO(o, false))
	}
	writeJSON(w, 200, map[string]any{"orders": result, "total": total, "limit": limit, "offset": offset})
}

type resellerOrderInput struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

func validIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}
func (a *App) resellerPlaceOrder(w http.ResponseWriter, r *http.Request, hash string, buyer int64) {
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if !validIdempotencyKey(idempotencyKey) {
		resellerError(w, 400, "invalid_idempotency_key", "Idempotency-Key is required: 1–128 letters, digits, -, _, . or :.")
		return
	}
	var input resellerOrderInput
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		resellerError(w, 400, "invalid_request", "Provide a JSON object with sku and quantity.")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		resellerError(w, 400, "invalid_request", "Provide exactly one JSON object.")
		return
	}
	if input.SKU == "" || len(input.SKU) > 100 || sku(input.SKU) != input.SKU || input.Quantity < 1 || input.Quantity > 50 {
		resellerError(w, 400, "invalid_request", "Use a catalog SKU and quantity from 1 to 50.")
		return
	}
	joined, err := a.channelMember(r.Context(), buyer)
	if err != nil {
		resellerError(w, 503, "membership_unavailable", "Channel membership could not be checked.")
		return
	}
	if !joined {
		resellerError(w, 403, "channel_required", "Join the updates channel in the bot before shopping.")
		return
	}
	o, replayed, err := a.purchaseResellerOrder(hash, buyer, idempotencyKey, input)
	if err != nil {
		var failure *resellerPurchaseError
		if errors.As(err, &failure) {
			resellerError(w, failure.Status, failure.Code, failure.Message)
		} else {
			resellerError(w, 500, "save_failed", "Could not save order. Retry using the same Idempotency-Key.")
		}
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, map[string]any{"order": resellerOrderDTO(o, true)})
}

type resellerPurchaseError struct {
	Status        int
	Code, Message string
}

func (e *resellerPurchaseError) Error() string { return e.Message }
func resellerPurchaseFailure(status int, code, message string) (Order, bool, error) {
	return Order{}, false, &resellerPurchaseError{status, code, message}
}

func (a *App) purchaseResellerOrder(hash string, buyer int64, requestKey string, input resellerOrderInput) (Order, bool, error) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	// Recheck after membership I/O: a revoked key must not debit a wallet.
	if key, ok := a.store.data.ResellerKeys[hash]; !ok || key.BuyerID != buyer {
		return resellerPurchaseFailure(401, "invalid_api_key", "API key is no longer active.")
	}
	for _, o := range a.store.data.Orders {
		if o.BuyerID == buyer && o.Reseller != nil && o.Reseller.IdempotencyKey == requestKey {
			if o.SKU != input.SKU || o.Quantity != input.Quantity {
				return resellerPurchaseFailure(409, "idempotency_conflict", "This Idempotency-Key was already used for a different order.")
			}
			return o, true, nil
		}
	}
	p, ok := a.store.data.Products[input.SKU]
	if !ok || !p.Active {
		return resellerPurchaseFailure(404, "product_not_found", "Product not found.")
	}
	cents := math.Round(p.PriceUSDT * 100)
	if math.IsNaN(cents) || math.IsInf(cents, 0) || cents < 1 || cents > 1e9 {
		return resellerPurchaseFailure(409, "invalid_price", "This product's price needs owner attention.")
	}
	candidates := []StockItem{}
	for _, item := range a.store.data.Stock {
		if item.SKU == p.SKU && !item.Sold && item.OrderID == "" {
			candidates = append(candidates, item)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].AddedAt.Equal(candidates[j].AddedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].AddedAt.Before(candidates[j].AddedAt)
	})
	if len(candidates) < input.Quantity {
		return resellerPurchaseFailure(409, "insufficient_stock", fmt.Sprintf("Only %d units are available.", len(candidates)))
	}
	cost := int64(cents) * int64(input.Quantity)
	account := a.store.data.Wallets[walletKey(buyer)]
	if account.BalanceCents < cost {
		return resellerPurchaseFailure(409, "insufficient_balance", "Add USDT funds through /wallet in the bot before placing this order.")
	}
	// A paid API order is delivered by this response (or a later GET), independently
	// of Telegram. Stock and delivery payloads are durable before any response.
	randomID := make([]byte, 16)
	if _, err := rand.Read(randomID); err != nil {
		return Order{}, false, err
	}
	now := time.Now().UTC()
	o := Order{ID: "ord_" + hex.EncodeToString(randomID), SKU: p.SKU, ProductName: p.Name, DeliveryInstruction: p.DeliveryInstruction, Quantity: input.Quantity, BuyerID: buyer, BuyerName: a.store.data.Started[buyer], Amount: float64(cost) / 100, Network: "wallet", Status: "delivered", CreatedAt: now, PaidAt: now, Reseller: &ResellerOrderData{IdempotencyKey: requestKey, AmountCents: cost}}
	for _, item := range candidates[:input.Quantity] {
		item.Sold = true
		item.OrderID = o.ID
		a.store.data.Stock[item.ID] = item
		o.StockIDs = append(o.StockIDs, item.ID)
		o.Reseller.Payloads = append(o.Reseller.Payloads, item.Payload)
	}
	account.BalanceCents -= cost
	account.Entries = append(account.Entries, WalletEntry{ID: id("ledger"), Kind: "purchase", Reference: o.ID, AmountCents: -cost, CreatedAt: now})
	a.store.data.Wallets[walletKey(buyer)] = account
	a.store.data.Orders[o.ID] = o
	if err := a.store.saveLocked(); err != nil {
		return Order{}, false, err
	}
	return o, false, nil
}
