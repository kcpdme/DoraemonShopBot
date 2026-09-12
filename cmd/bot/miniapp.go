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
	writeJSON(w, http.StatusOK, map[string]any{"products": products, "channelUrl": a.channelURL()})
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
	if publicURL, parseErr := url.Parse(a.cfg.MiniAppURL); parseErr == nil {
		prefix := strings.TrimSuffix(publicURL.Path, "/")
		if prefix != "" {
			mux.HandleFunc(prefix+"/api/mini-app/catalog", a.miniAppCatalog)
			mux.HandleFunc(prefix+"/api/mini-app/orders", a.miniAppOrders)
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
