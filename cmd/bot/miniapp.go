package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const miniAppAuthLifetime = time.Hour

//go:embed miniapp/*
var miniAppFiles embed.FS

type miniAppProduct struct {
	SKU         string  `json:"sku"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	PriceUSDT   float64 `json:"priceUsdt"`
	Stock       int     `json:"stock"`
}

type miniAppOrder struct {
	ID        string    `json:"id"`
	SKU       string    `json:"sku"`
	Name      string    `json:"name"`
	Quantity  int       `json:"quantity"`
	Amount    float64   `json:"amount"`
	Network   string    `json:"network"`
	Wallet    string    `json:"wallet,omitempty"`
	Status    string    `json:"status"`
	TxHash    string    `json:"txHash,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type miniAppAdminProduct struct {
	miniAppProduct
	Active              bool   `json:"active"`
	DeliveryInstruction string `json:"deliveryInstruction"`
}

type miniAppStockItem struct {
	ID      string    `json:"id"`
	SKU     string    `json:"sku"`
	State   string    `json:"state"`
	AddedAt time.Time `json:"addedAt"`
}

func validateMiniAppURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) {
		return errors.New("MINI_APP_PUBLIC_URL must be an HTTPS URL (HTTP is allowed only for localhost development)")
	}
	return nil
}

func validateTelegramInitData(raw, token string, now time.Time) (User, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return User{}, errors.New("invalid Telegram launch data")
	}
	provided, err := hex.DecodeString(values.Get("hash"))
	if err != nil || len(provided) != sha256.Size {
		return User{}, errors.New("invalid Telegram launch signature")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "hash" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+values.Get(key))
	}
	secretMAC := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secretMAC.Write([]byte(token))
	signatureMAC := hmac.New(sha256.New, secretMAC.Sum(nil))
	_, _ = signatureMAC.Write([]byte(strings.Join(lines, "\n")))
	if subtle.ConstantTimeCompare(provided, signatureMAC.Sum(nil)) != 1 {
		return User{}, errors.New("invalid Telegram launch signature")
	}
	authUnix, err := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if err != nil || authUnix <= 0 {
		return User{}, errors.New("invalid Telegram launch time")
	}
	authTime := time.Unix(authUnix, 0)
	if authTime.After(now.Add(time.Minute)) || now.Sub(authTime) > miniAppAuthLifetime {
		return User{}, errors.New("Telegram launch data has expired; reopen the Mini App")
	}
	var user User
	if err := json.Unmarshal([]byte(values.Get("user")), &user); err != nil || user.ID <= 0 {
		return User{}, errors.New("Telegram user is missing from launch data")
	}
	return user, nil
}

func telegramInitData(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(value, "tma ") {
		return strings.TrimSpace(strings.TrimPrefix(value, "tma "))
	}
	return r.Header.Get("X-Telegram-Init-Data")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (a *App) miniAppUser(w http.ResponseWriter, r *http.Request) (User, bool) {
	user, err := validateTelegramInitData(telegramInitData(r), a.cfg.Token, time.Now())
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, err)
		return User{}, false
	}
	return user, true
}

// miniAppOwner is the authorization boundary for the Mini App admin tools.
// The UI may hide them for non-owners, but every mutating request is checked
// here using signed Telegram launch data.
func (a *App) miniAppOwner(w http.ResponseWriter, r *http.Request) (User, bool) {
	user, ok := a.miniAppUser(w, r)
	if !ok {
		return User{}, false
	}
	if !admin(a, user.ID) {
		writeAPIError(w, http.StatusForbidden, errors.New("owner access required"))
		return User{}, false
	}
	return user, true
}

func (a *App) miniAppCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	user, ok := a.miniAppUser(w, r)
	if !ok {
		return
	}
	joined, err := a.channelMember(r.Context(), user.ID)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, errors.New("channel membership could not be checked"))
		return
	}
	if !joined {
		writeAPIError(w, http.StatusForbidden, errors.New("join the updates channel in the bot before shopping"))
		return
	}
	a.remember(&user)
	a.store.mu.Lock()
	products := make([]miniAppProduct, 0, len(a.store.data.Products))
	for _, product := range a.store.data.Products {
		if !product.Active {
			continue
		}
		stock := 0
		for _, item := range a.store.data.Stock {
			if item.SKU == product.SKU && !item.Sold && item.OrderID == "" {
				stock++
			}
		}
		products = append(products, miniAppProduct{SKU: product.SKU, Name: product.Name, Description: product.Description, PriceUSDT: product.PriceUSDT, Stock: stock})
	}
	a.store.mu.Unlock()
	sort.Slice(products, func(i, j int) bool { return products[i].Name < products[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"products": products, "channelUrl": a.channelURL(), "isOwner": admin(a, user.ID)})
}

func miniAppStockState(item StockItem) string {
	if item.Sold {
		return "sold"
	}
	if item.OrderID != "" {
		return "reserved"
	}
	return "available"
}

func (a *App) miniAppAdminProducts(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.miniAppOwner(w, r); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		a.store.mu.Lock()
		products := make([]miniAppAdminProduct, 0, len(a.store.data.Products))
		for _, product := range a.store.data.Products {
			stock := 0
			for _, item := range a.store.data.Stock {
				if item.SKU == product.SKU && !item.Sold && item.OrderID == "" {
					stock++
				}
			}
			products = append(products, miniAppAdminProduct{miniAppProduct: miniAppProduct{SKU: product.SKU, Name: product.Name, Description: product.Description, PriceUSDT: product.PriceUSDT, Stock: stock}, Active: product.Active, DeliveryInstruction: product.DeliveryInstruction})
		}
		a.store.mu.Unlock()
		sort.Slice(products, func(i, j int) bool { return products[i].Name < products[j].Name })
		writeJSON(w, http.StatusOK, map[string]any{"products": products})
	case http.MethodPost:
		var input struct {
			SKU                 string  `json:"sku"`
			Name                string  `json:"name"`
			Description         string  `json:"description"`
			PriceUSDT           float64 `json:"priceUsdt"`
			DeliveryInstruction string  `json:"deliveryInstruction"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 12288)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeAPIError(w, http.StatusBadRequest, errors.New("invalid product request"))
			return
		}
		input.SKU = sku(input.SKU)
		input.Name, input.Description, input.DeliveryInstruction = strings.TrimSpace(input.Name), strings.TrimSpace(input.Description), strings.TrimSpace(input.DeliveryInstruction)
		priceCents := math.Round(input.PriceUSDT * 100)
		if input.SKU == "" || len(input.Name) > 120 || len(input.Description) > 2000 || len(input.DeliveryInstruction) == 0 || len(input.DeliveryInstruction) > 12000 || math.IsNaN(priceCents) || math.IsInf(priceCents, 0) || priceCents < 1 || priceCents > 1e9 {
			writeAPIError(w, http.StatusBadRequest, errors.New("provide a SKU, name, price, and delivery instructions within the allowed size"))
			return
		}
		a.store.mu.Lock()
		if _, exists := a.store.data.Products[input.SKU]; exists {
			a.store.mu.Unlock()
			writeAPIError(w, http.StatusConflict, errors.New("that SKU already exists"))
			return
		}
		product := Product{SKU: input.SKU, Name: input.Name, Description: input.Description, PriceUSDT: priceCents / 100, DeliveryInstruction: input.DeliveryInstruction, Active: true, CreatedAt: time.Now().UTC()}
		a.store.data.Products[product.SKU] = product
		err := a.store.saveLocked()
		a.store.mu.Unlock()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, errors.New("could not save product"))
			return
		}
		writeJSON(w, http.StatusCreated, miniAppAdminProduct{miniAppProduct: miniAppProduct{SKU: product.SKU, Name: product.Name, Description: product.Description, PriceUSDT: product.PriceUSDT}, Active: product.Active, DeliveryInstruction: product.DeliveryInstruction})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (a *App) miniAppAdminProductItem(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.miniAppOwner(w, r); !ok {
		return
	}
	if r.Method != http.MethodPatch {
		writeAPIError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	const productPath = "/api/mini-app/admin/products/"
	position := strings.Index(r.URL.Path, productPath)
	productSKU := ""
	if position >= 0 {
		productSKU = sku(r.URL.Path[position+len(productPath):])
	}
	if productSKU == "" {
		writeAPIError(w, http.StatusBadRequest, errors.New("invalid product"))
		return
	}
	var input struct {
		PriceUSDT *float64 `json:"priceUsdt"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.PriceUSDT == nil {
		writeAPIError(w, http.StatusBadRequest, errors.New("provide a price in USDT"))
		return
	}
	priceCents := math.Round(*input.PriceUSDT * 100)
	if math.IsNaN(priceCents) || math.IsInf(priceCents, 0) || priceCents < 1 || priceCents > 1e9 {
		writeAPIError(w, http.StatusBadRequest, errors.New("provide a valid price in USDT"))
		return
	}
	a.store.mu.Lock()
	product, exists := a.store.data.Products[productSKU]
	if !exists {
		a.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, errors.New("product not found"))
		return
	}
	product.PriceUSDT = priceCents / 100
	a.store.data.Products[productSKU] = product
	err := a.store.saveLocked()
	stock := 0
	for _, item := range a.store.data.Stock {
		if item.SKU == productSKU && !item.Sold && item.OrderID == "" {
			stock++
		}
	}
	a.store.mu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errors.New("could not update product price"))
		return
	}
	writeJSON(w, http.StatusOK, miniAppAdminProduct{miniAppProduct: miniAppProduct{SKU: product.SKU, Name: product.Name, Description: product.Description, PriceUSDT: product.PriceUSDT, Stock: stock}, Active: product.Active, DeliveryInstruction: product.DeliveryInstruction})
}

func (a *App) miniAppAdminStock(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.miniAppOwner(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if r.Method == http.MethodGet {
		code := sku(r.URL.Query().Get("sku"))
		a.store.mu.Lock()
		items := make([]miniAppStockItem, 0)
		for _, item := range a.store.data.Stock {
			if code == "" || item.SKU == code {
				items = append(items, miniAppStockItem{ID: item.ID, SKU: item.SKU, State: miniAppStockState(item), AddedAt: item.AddedAt})
			}
		}
		a.store.mu.Unlock()
		sort.Slice(items, func(i, j int) bool { return items[i].AddedAt.After(items[j].AddedAt) })
		writeJSON(w, http.StatusOK, map[string]any{"stock": items})
		return
	}
	var input struct {
		SKU      string `json:"sku"`
		Payloads string `json:"payloads"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(w, http.StatusBadRequest, errors.New("invalid stock request"))
		return
	}
	input.SKU = sku(input.SKU)
	payloads := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(input.Payloads, "\r\n", "\n"), "\n") {
		if value := strings.TrimSpace(line); value != "" {
			payloads = append(payloads, value)
		}
	}
	if input.SKU == "" || len(payloads) == 0 || len(payloads) > 500 {
		writeAPIError(w, http.StatusBadRequest, errors.New("choose a product and provide up to 500 non-empty stock lines"))
		return
	}
	a.store.mu.Lock()
	if _, exists := a.store.data.Products[input.SKU]; !exists {
		a.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, errors.New("product not found"))
		return
	}
	for _, payload := range payloads {
		item := StockItem{ID: id("stock"), SKU: input.SKU, Payload: payload, AddedAt: time.Now().UTC()}
		a.store.data.Stock[item.ID] = item
	}
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errors.New("could not save stock"))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"added": len(payloads)})
}

func (a *App) miniAppAdminStockItem(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.miniAppOwner(w, r); !ok {
		return
	}
	if r.Method != http.MethodDelete {
		writeAPIError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	const stockPath = "/api/mini-app/admin/stock/"
	position := strings.Index(r.URL.Path, stockPath)
	stockID := ""
	if position >= 0 {
		stockID = r.URL.Path[position+len(stockPath):]
	}
	if stockID == "" || strings.Contains(stockID, "/") {
		writeAPIError(w, http.StatusBadRequest, errors.New("invalid stock item"))
		return
	}
	a.store.mu.Lock()
	item, exists := a.store.data.Stock[stockID]
	if !exists {
		a.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, errors.New("stock item not found"))
		return
	}
	if item.Sold || item.OrderID != "" {
		a.store.mu.Unlock()
		writeAPIError(w, http.StatusConflict, errors.New("only available stock can be removed"))
		return
	}
	delete(a.store.data.Stock, stockID)
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errors.New("could not remove stock"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) miniAppOrders(w http.ResponseWriter, r *http.Request) {
	user, ok := a.miniAppUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		a.store.mu.Lock()
		orders := make([]miniAppOrder, 0)
		for _, order := range a.store.data.Orders {
			if order.BuyerID == user.ID {
				orders = append(orders, a.publicMiniAppOrder(order))
			}
		}
		a.store.mu.Unlock()
		sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt.After(orders[j].CreatedAt) })
		writeJSON(w, http.StatusOK, map[string]any{"orders": orders})
	case http.MethodPost:
		joined, err := a.channelMember(r.Context(), user.ID)
		if err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, errors.New("channel membership could not be checked"))
			return
		}
		if !joined {
			writeAPIError(w, http.StatusForbidden, errors.New("join the updates channel in the bot before shopping"))
			return
		}
		var input struct {
			SKU      string `json:"sku"`
			Quantity int    `json:"quantity"`
			Network  string `json:"network"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeAPIError(w, http.StatusBadRequest, errors.New("invalid order request"))
			return
		}
		input.SKU = sku(input.SKU)
		order, err := a.createOrderQuantity(&user, input.SKU, input.Network, input.Quantity)
		if err != nil {
			writeAPIError(w, http.StatusConflict, err)
			return
		}
		a.remember(&user)
		if err := a.sendPaymentCard(r.Context(), user.ID, order, order.PaymentMessageID); err != nil {
			log.Printf("Mini App payment card %s: %v", order.ID, err)
		}
		writeJSON(w, http.StatusCreated, map[string]any{"order": a.publicMiniAppOrder(order)})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (a *App) publicMiniAppOrder(order Order) miniAppOrder {
	wallet := ""
	expiresAt := order.CreatedAt.Add(30 * time.Minute)
	if (order.Status == "awaiting_payment" || order.Status == "proof_invalid") && time.Now().Before(expiresAt) {
		wallet = a.cfg.BEP20Address
		if order.Network == "polygon" {
			wallet = a.cfg.PolygonAddress
		}
	}
	return miniAppOrder{ID: order.ID, SKU: order.SKU, Name: order.ProductName, Quantity: max(1, order.Quantity), Amount: order.Amount, Network: order.Network, Wallet: wallet, Status: friendlyOrderStatus(order.Status), TxHash: order.TxHash, CreatedAt: order.CreatedAt, ExpiresAt: expiresAt}
}

func miniAppSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' https://telegram.org; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors https://web.telegram.org https://*.telegram.org")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func miniAppStaticHandler(files fs.FS) http.Handler {
	server := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		requested := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if requested == "." {
			requested = "index.html"
		}
		if info, err := fs.Stat(files, requested); err != nil || info.IsDir() {
			clone := r.Clone(r.Context())
			clonedURL := *r.URL
			clonedURL.Path = "/"
			clone.URL = &clonedURL
			server.ServeHTTP(w, clone)
			return
		}
		server.ServeHTTP(w, r)
	})
}

func (a *App) miniAppHandler() (http.Handler, error) {
	static, err := fs.Sub(miniAppFiles, "miniapp")
	if err != nil {
		return nil, fmt.Errorf("load Mini App files: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/mini-app/catalog", a.miniAppCatalog)
	mux.HandleFunc("/api/mini-app/orders", a.miniAppOrders)
	mux.HandleFunc("/api/mini-app/admin/products", a.miniAppAdminProducts)
	mux.HandleFunc("/api/mini-app/admin/products/", a.miniAppAdminProductItem)
	mux.HandleFunc("/api/mini-app/admin/stock", a.miniAppAdminStock)
	mux.HandleFunc("/api/mini-app/admin/stock/", a.miniAppAdminStockItem)
	if publicURL, parseErr := url.Parse(a.cfg.MiniAppURL); parseErr == nil {
		prefix := strings.TrimSuffix(publicURL.Path, "/")
		if prefix != "" {
			mux.HandleFunc(prefix+"/api/mini-app/catalog", a.miniAppCatalog)
			mux.HandleFunc(prefix+"/api/mini-app/orders", a.miniAppOrders)
			mux.HandleFunc(prefix+"/api/mini-app/admin/products", a.miniAppAdminProducts)
			mux.HandleFunc(prefix+"/api/mini-app/admin/products/", a.miniAppAdminProductItem)
			mux.HandleFunc(prefix+"/api/mini-app/admin/stock", a.miniAppAdminStock)
			mux.HandleFunc(prefix+"/api/mini-app/admin/stock/", a.miniAppAdminStockItem)
			mux.Handle(prefix+"/", http.StripPrefix(prefix, miniAppStaticHandler(static)))
		}
	}
	mux.Handle("/", miniAppStaticHandler(static))
	return miniAppSecurityHeaders(mux), nil
}

func (a *App) startMiniAppServer(ctx context.Context) error {
	handler, err := a.miniAppHandler()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", a.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen for Mini App on %s: %w", a.cfg.HTTPAddr, err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		log.Printf("Telegram Mini App listening on %s", a.cfg.HTTPAddr)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Mini App server: %v", err)
		}
	}()
	return nil
}
