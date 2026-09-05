// Doraemon Shop Bot is a Telegram-only digital-goods store.
// It deliberately uses the Telegram Bot API directly so deployment needs no web UI.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Token          string
	OwnerID        int64
	Channel        string // @channel or numeric channel id; bot must be an administrator.
	BEP20Address   string
	PolygonAddress string
	ExplorerAPIKey string
	BEP20USDT      string
	PolygonUSDT    string
	DataFile       string
}

func config() (Config, error) {
	owner, err := strconv.ParseInt(os.Getenv("OWNER_TELEGRAM_ID"), 10, 64)
	if err != nil || owner == 0 {
		return Config{}, errors.New("OWNER_TELEGRAM_ID must be a numeric Telegram user ID")
	}
	c := Config{Token: os.Getenv("BOT_TOKEN"), OwnerID: owner, Channel: os.Getenv("SALES_CHANNEL"), BEP20Address: os.Getenv("BEP20_USDT_ADDRESS"), PolygonAddress: os.Getenv("POLYGON_USDT_ADDRESS"), ExplorerAPIKey: os.Getenv("ETHERSCAN_API_KEY"), BEP20USDT: os.Getenv("BEP20_USDT_CONTRACT"), PolygonUSDT: os.Getenv("POLYGON_USDT_CONTRACT"), DataFile: os.Getenv("DATA_FILE")}
	if c.Token == "" || c.BEP20Address == "" || c.PolygonAddress == "" {
		return Config{}, errors.New("BOT_TOKEN, BEP20_USDT_ADDRESS and POLYGON_USDT_ADDRESS are required")
	}
	if c.ExplorerAPIKey == "" || c.BEP20USDT == "" || c.PolygonUSDT == "" {
		return Config{}, errors.New("ETHERSCAN_API_KEY, BEP20_USDT_CONTRACT and POLYGON_USDT_CONTRACT are required for automatic verification")
	}
	if c.DataFile == "" {
		c.DataFile = "data/store.json"
	}
	return c, nil
}

type Product struct {
	SKU, Name, Description, DeliveryInstruction string
	PriceUSDT                                   float64
	Active                                      bool
	CreatedAt                                   time.Time
}
type StockItem struct {
	ID, SKU, Payload string
	OrderID          string
	AddedAt          time.Time
	Sold             bool
}
type Order struct {
	ID, SKU                 string
	Quantity                int
	BuyerID                 int64
	BuyerName               string
	Amount                  float64
	Network, TxHash, Status string
	StockIDs                []string
	CreatedAt, PaidAt       time.Time
}
type StoreData struct {
	Products map[string]Product
	Stock    map[string]StockItem
	Orders   map[string]Order
	Started  map[int64]string
}
type Store struct {
	mu   sync.Mutex
	path string
	data StoreData
}

func openStore(path string) (*Store, error) {
	s := &Store{path: path, data: StoreData{Products: map[string]Product{}, Stock: map[string]StockItem{}, Orders: map[string]Order{}, Started: map[int64]string{}}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}
	if s.data.Products == nil {
		s.data.Products = map[string]Product{}
	}
	if s.data.Stock == nil {
		s.data.Stock = map[string]StockItem{}
	}
	if s.data.Orders == nil {
		s.data.Orders = map[string]Order{}
	}
	if s.data.Started == nil {
		s.data.Started = map[int64]string{}
	}
	return s, nil
}
func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
func (s *Store) save() error { s.mu.Lock(); defer s.mu.Unlock(); return s.saveLocked() }

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}
type Chat struct {
	ID int64 `json:"id"`
}
type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from"`
	Text      string `json:"text"`
}
type Callback struct {
	ID, Data string
	From     User     `json:"from"`
	Message  *Message `json:"message"`
}
type Update struct {
	UpdateID int64     `json:"update_id"`
	Message  *Message  `json:"message"`
	Callback *Callback `json:"callback_query"`
}
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}
type Markup struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}
type TG struct {
	token  string
	client *http.Client
}

func (t *TG) call(ctx context.Context, method string, v any, out any) error {
	b, _ := json.Marshal(v)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+t.token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	var reply struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &reply) != nil || !reply.OK {
		return fmt.Errorf("telegram %s: %s", method, reply.Description)
	}
	if out != nil {
		return json.Unmarshal(reply.Result, out)
	}
	return nil
}
func (t *TG) send(ctx context.Context, chat any, text string, kb *Markup) error {
	v := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if kb != nil {
		v["reply_markup"] = kb
	}
	return t.call(ctx, "sendMessage", v, nil)
}
func (t *TG) answer(ctx context.Context, id string) {
	_ = t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id}, nil)
}

type App struct {
	cfg   Config
	store *Store
	tg    *TG
}

// ERC-20 transfers returned by Etherscan's multichain V2 API.
type tokenTransfer struct {
	Hash          string `json:"hash"`
	To            string `json:"to"`
	Contract      string `json:"contractAddress"`
	Value         string `json:"value"`
	Decimals      string `json:"tokenDecimal"`
	Confirmations string `json:"confirmations"`
}

// checkPayment returns (settled, reason, error). A non-settled valid pending
// transaction remains eligible for the next poll; it is never auto-rejected.
func (a *App) checkPayment(ctx context.Context, o Order) (bool, string, error) {
	chainID, wallet, token := "56", a.cfg.BEP20Address, a.cfg.BEP20USDT
	if o.Network == "polygon" {
		chainID, wallet, token = "137", a.cfg.PolygonAddress, a.cfg.PolygonUSDT
	}
	q := url.Values{
		"chainid": {chainID}, "module": {"account"}, "action": {"tokentx"},
		"address": {wallet}, "contractaddress": {token}, "page": {"1"},
		"offset": {"100"}, "sort": {"desc"}, "apikey": {a.cfg.ExplorerAPIKey},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.etherscan.io/v2/api?"+q.Encode(), nil)
	if err != nil {
		return false, "", err
	}
	r, err := a.tg.client.Do(req)
	if err != nil {
		return false, "", err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("explorer status %d", r.StatusCode)
	}
	var reply struct {
		Status, Message string
		Result          json.RawMessage
	}
	if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
		return false, "", err
	}
	if reply.Status != "1" {
		// "No transactions found" is normal while a transaction is pending/indexing.
		if strings.Contains(strings.ToLower(reply.Message), "no transactions") {
			return false, "waiting for chain indexing", nil
		}
		return false, "", fmt.Errorf("explorer: %s", reply.Message)
	}
	var transfers []tokenTransfer
	if err := json.Unmarshal(reply.Result, &transfers); err != nil {
		return false, "", err
	}
	for _, tx := range transfers {
		if !strings.EqualFold(tx.Hash, o.TxHash) {
			continue
		}
		if !strings.EqualFold(tx.To, wallet) || !strings.EqualFold(tx.Contract, token) {
			return false, "transfer target does not match", nil
		}
		if !exactUSDTAmount(o.Amount, tx.Value, tx.Decimals) {
			return false, "transfer amount does not match", nil
		}
		confirmations, err := strconv.Atoi(tx.Confirmations)
		if err != nil || confirmations < 3 {
			return false, "waiting for 3 confirmations", nil
		}
		if err := a.transactionSucceeded(ctx, chainID, o.TxHash); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	return false, "transaction not found yet", nil
}

func (a *App) transactionSucceeded(ctx context.Context, chainID, hash string) error {
	q := url.Values{"chainid": {chainID}, "module": {"transaction"}, "action": {"gettxreceiptstatus"}, "txhash": {hash}, "apikey": {a.cfg.ExplorerAPIKey}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.etherscan.io/v2/api?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	r, err := a.tg.client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	var reply struct {
		Status string
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
		return err
	}
	if reply.Status != "1" || reply.Result.Status != "1" {
		return errors.New("transaction failed or receipt unavailable")
	}
	return nil
}

func exactUSDTAmount(price float64, raw, decimals string) bool {
	d, err := strconv.Atoi(decimals)
	if err != nil || d < 2 {
		return false
	}
	cents := new(big.Int)
	cents.SetString(fmt.Sprintf("%.0f", price*100), 10)
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d-2)), nil)
	expected := new(big.Int).Mul(cents, factor)
	actual, ok := new(big.Int).SetString(raw, 10)
	return ok && actual.Cmp(expected) == 0
}

func admin(c *App, id int64) bool { return id == c.cfg.OwnerID }
func esc(s string) string         { return html.EscapeString(s) }
func id(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%x", prefix, b)
}
func sku(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func userLabel(u *User) string {
	if u == nil {
		return "Customer"
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	if u.FirstName != "" {
		return u.FirstName
	}
	return "Customer"
}

func (a *App) remember(u *User) {
	if u == nil {
		return
	}
	a.store.mu.Lock()
	a.store.data.Started[u.ID] = userLabel(u)
	_ = a.store.saveLocked()
	a.store.mu.Unlock()
}
func (a *App) productsText() (string, *Markup) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	ps := []Product{}
	for _, p := range a.store.data.Products {
		if p.Active {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].CreatedAt.After(ps[j].CreatedAt) })
	if len(ps) == 0 {
		return "🛍 <b>Shop</b>\n\nNo products are available right now.", nil
	}
	text := "🛍 <b>Available digital goods</b>\n\nTap a product to see its description and stock."
	rows := [][]Button{}
	for _, p := range ps {
		n := 0
		for _, x := range a.store.data.Stock {
			if x.SKU == p.SKU && !x.Sold && x.OrderID == "" {
				n++
			}
		}
		rows = append(rows, []Button{{Text: fmt.Sprintf("%s · %.2f USDT · %d left", p.Name, p.PriceUSDT, n), Data: "product:" + p.SKU}})
	}
	return text, &Markup{InlineKeyboard: rows}
}
func (a *App) productText(code string) (string, *Markup) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	p, ok := a.store.data.Products[code]
	if !ok || !p.Active {
		return "This product is no longer available.", nil
	}
	n := 0
	for _, x := range a.store.data.Stock {
		if x.SKU == code && !x.Sold && x.OrderID == "" {
			n++
		}
	}
	text := fmt.Sprintf("<b>%s</b>\n\n%s\n\nPrice: <b>%.2f USDT</b>\nIn stock: <b>%d</b>\n\nDelivery instructions are sent only after payment is confirmed.", esc(p.Name), esc(p.Description), p.PriceUSDT, n)
	return text, &Markup{InlineKeyboard: [][]Button{{{Text: "Buy with USDT", Data: "buy:" + code}}, {{Text: "← Catalog", Data: "catalog"}}}}
}
func (a *App) createOrder(buyer *User, code, network string) (Order, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	p, ok := a.store.data.Products[code]
	if !ok || !p.Active {
		return Order{}, errors.New("product unavailable")
	}
	candidates := []string{}
	for k, x := range a.store.data.Stock {
		if x.SKU == code && !x.Sold && x.OrderID == "" {
			candidates = append(candidates, k)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return Order{}, errors.New("sold out")
	}
	o := Order{ID: id("ord"), SKU: code, Quantity: 1, BuyerID: buyer.ID, BuyerName: userLabel(buyer), Amount: p.PriceUSDT, Network: network, Status: "awaiting_payment", StockIDs: candidates[:1], CreatedAt: time.Now().UTC()}
	for _, k := range o.StockIDs {
		x := a.store.data.Stock[k]
		x.OrderID = o.ID
		a.store.data.Stock[k] = x
	}
	a.store.data.Orders[o.ID] = o
	return o, a.store.saveLocked()
}
func (a *App) paymentText(o Order) string {
	addr := a.cfg.BEP20Address
	if o.Network == "polygon" {
		addr = a.cfg.PolygonAddress
	}
	return fmt.Sprintf("<b>Order %s</b>\n\nSend exactly <b>%.2f USDT</b> on <b>%s</b> to:\n<code>%s</code>\n\nAfter the transfer, use:\n<code>/paid %s YOUR_TRANSACTION_HASH</code>\n\nYour reserved stock is held for 30 minutes. Do not use another network.", esc(o.ID), o.Amount, map[string]string{"bep20": "BEP-20 (BNB Smart Chain)", "polygon": "Polygon"}[o.Network], esc(addr), o.ID)
}
func (a *App) broadcast(ctx context.Context, msg string, includeChannel bool) {
	a.store.mu.Lock()
	ids := make([]int64, 0, len(a.store.data.Started))
	for k := range a.store.data.Started {
		ids = append(ids, k)
	}
	a.store.mu.Unlock()
	for _, chat := range ids {
		if err := a.tg.send(ctx, chat, msg, nil); err != nil {
			log.Printf("broadcast %d: %v", chat, err)
		}
	}
	if includeChannel && a.cfg.Channel != "" {
		if err := a.tg.send(ctx, a.cfg.Channel, msg, nil); err != nil {
			log.Printf("channel: %v", err)
		}
	}
}

func (a *App) handleMessage(ctx context.Context, m *Message) {
	if m == nil || m.From == nil {
		return
	}
	a.remember(m.From)
	text := strings.TrimSpace(m.Text)
	parts := strings.Fields(text)
	cmd := ""
	if len(parts) > 0 {
		cmd = strings.Split(parts[0], "@")[0]
	}
	switch cmd {
	case "/start":
		a.tg.send(ctx, m.Chat.ID, "Welcome to <b>Doraemon Shop</b>. Browse in-stock digital goods and receive delivery details securely after payment confirmation.", &Markup{InlineKeyboard: [][]Button{{{Text: "🛍 Browse products", Data: "catalog"}}}})
	case "/shop":
		t, k := a.productsText()
		a.tg.send(ctx, m.Chat.ID, t, k)
	case "/help":
		a.tg.send(ctx, m.Chat.ID, "/shop — browse products\n/orders — your orders\n/paid ORDER_ID TX_HASH — submit a payment hash", nil)
	case "/orders":
		a.sendOrders(ctx, m.Chat.ID, m.From.ID)
	case "/paid":
		a.submitPaid(ctx, m, parts)
	case "/admin":
		if admin(a, m.From.ID) {
			a.adminHelp(ctx, m.Chat.ID)
		}
	case "/addproduct":
		if admin(a, m.From.ID) {
			a.addProduct(ctx, m, strings.TrimSpace(strings.TrimPrefix(text, "/addproduct")))
		}
	case "/stock":
		if admin(a, m.From.ID) {
			a.addStock(ctx, m, strings.TrimSpace(strings.TrimPrefix(text, "/stock")))
		}
	case "/confirm":
		if admin(a, m.From.ID) {
			a.confirm(ctx, m, parts)
		}
	case "/reject":
		if admin(a, m.From.ID) {
			a.reject(ctx, m, parts)
		}
	case "/announce":
		if admin(a, m.From.ID) {
			msg := strings.TrimSpace(strings.TrimPrefix(text, "/announce"))
			if msg != "" {
				a.broadcast(ctx, esc(msg), true)
				a.tg.send(ctx, m.Chat.ID, "Announcement sent.", nil)
			}
		}
	}
}
func (a *App) sendOrders(ctx context.Context, chat, buyer int64) {
	a.store.mu.Lock()
	os := []Order{}
	for _, o := range a.store.data.Orders {
		if o.BuyerID == buyer {
			os = append(os, o)
		}
	}
	a.store.mu.Unlock()
	sort.Slice(os, func(i, j int) bool { return os[i].CreatedAt.After(os[j].CreatedAt) })
	if len(os) == 0 {
		a.tg.send(ctx, chat, "No orders yet.", nil)
		return
	}
	var b strings.Builder
	b.WriteString("<b>Your orders</b>\n\n")
	for _, o := range os {
		b.WriteString(fmt.Sprintf("<code>%s</code> · %s · %.2f USDT · <b>%s</b>\n", o.ID, esc(o.SKU), o.Amount, esc(o.Status)))
	}
	a.tg.send(ctx, chat, b.String(), nil)
}
func (a *App) submitPaid(ctx context.Context, m *Message, p []string) {
	if len(p) != 3 {
		a.tg.send(ctx, m.Chat.ID, "Usage: <code>/paid ORDER_ID TRANSACTION_HASH</code>", nil)
		return
	}
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[p[1]]
	if !ok || o.BuyerID != m.From.ID {
		a.store.mu.Unlock()
		a.tg.send(ctx, m.Chat.ID, "Order not found.", nil)
		return
	}
	if o.Status != "awaiting_payment" {
		a.store.mu.Unlock()
		a.tg.send(ctx, m.Chat.ID, "This order cannot accept a payment hash.", nil)
		return
	}
	for _, existing := range a.store.data.Orders {
		if existing.ID != o.ID && strings.EqualFold(existing.TxHash, p[2]) {
			a.store.mu.Unlock()
			a.tg.send(ctx, m.Chat.ID, "That transaction hash is already attached to another order.", nil)
			return
		}
	}
	o.TxHash = p[2]
	o.Status = "payment_submitted"
	a.store.data.Orders[o.ID] = o
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		a.tg.send(ctx, m.Chat.ID, "Could not save payment proof. Try again.", nil)
		return
	}
	a.tg.send(ctx, m.Chat.ID, "Payment hash submitted. We are checking the blockchain now; delivery is automatic after three confirmations.", nil)
	go a.verifyOrder(context.Background(), o.ID)
}
func (a *App) adminHelp(ctx context.Context, chat int64) {
	a.tg.send(ctx, chat, "<b>Admin commands</b>\n\n<code>/addproduct SKU | Name | price_usdt | description | delivery instruction</code>\n<code>/stock SKU | one delivery payload</code>\n<code>/confirm ORDER_ID</code>\n<code>/reject ORDER_ID reason</code>\n<code>/announce message</code>\n\nEach /stock command adds one FIFO delivery unit and announces the restock to started users and the sales channel.", nil)
}
func (a *App) addProduct(ctx context.Context, m *Message, raw string) {
	x := strings.Split(raw, "|")
	if len(x) != 5 {
		a.tg.send(ctx, m.Chat.ID, "Use: <code>/addproduct SKU | Name | price_usdt | description | delivery instruction</code>", nil)
		return
	}
	code := sku(x[0])
	price, err := strconv.ParseFloat(strings.TrimSpace(x[2]), 64)
	if code == "" || err != nil || price <= 0 {
		a.tg.send(ctx, m.Chat.ID, "SKU and positive price are required.", nil)
		return
	}
	a.store.mu.Lock()
	a.store.data.Products[code] = Product{SKU: code, Name: strings.TrimSpace(x[1]), PriceUSDT: price, Description: strings.TrimSpace(x[3]), DeliveryInstruction: strings.TrimSpace(x[4]), Active: true, CreatedAt: time.Now().UTC()}
	err = a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		a.tg.send(ctx, m.Chat.ID, "Save failed.", nil)
		return
	}
	a.tg.send(ctx, m.Chat.ID, "Product "+esc(code)+" saved.", nil)
}
func (a *App) addStock(ctx context.Context, m *Message, raw string) {
	x := strings.SplitN(raw, "|", 2)
	if len(x) != 2 {
		a.tg.send(ctx, m.Chat.ID, "Use: <code>/stock SKU | one delivery payload</code>", nil)
		return
	}
	code := sku(x[0])
	payload := strings.TrimSpace(x[1])
	a.store.mu.Lock()
	p, ok := a.store.data.Products[code]
	if !ok || payload == "" {
		a.store.mu.Unlock()
		a.tg.send(ctx, m.Chat.ID, "Unknown SKU or empty payload.", nil)
		return
	}
	st := StockItem{ID: id("stock"), SKU: code, Payload: payload, AddedAt: time.Now().UTC()}
	a.store.data.Stock[st.ID] = st
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		a.tg.send(ctx, m.Chat.ID, "Save failed.", nil)
		return
	}
	a.broadcast(ctx, fmt.Sprintf("📦 <b>Restocked</b>\n\n%s is available for <b>%.2f USDT</b>.\nOpen the bot and tap /shop to buy.", esc(p.Name), p.PriceUSDT), true)
	a.tg.send(ctx, m.Chat.ID, "Stock added and restock announcement sent.", nil)
}
func (a *App) confirm(ctx context.Context, m *Message, p []string) {
	if len(p) != 2 {
		a.tg.send(ctx, m.Chat.ID, "Usage: <code>/confirm ORDER_ID</code>", nil)
		return
	}
	if err := a.deliverOrder(ctx, p[1]); err != nil {
		a.tg.send(ctx, m.Chat.ID, "Could not deliver: "+esc(err.Error()), nil)
		return
	}
	a.tg.send(ctx, m.Chat.ID, "Order confirmed and delivery sent.", nil)
}

func (a *App) deliverOrder(ctx context.Context, orderID string) error {
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	if !ok || (o.Status != "payment_submitted" && o.Status != "delivery_pending") {
		a.store.mu.Unlock()
		return errors.New("order is not awaiting verification")
	}
	prod, ok := a.store.data.Products[o.SKU]
	if !ok {
		a.store.mu.Unlock()
		return errors.New("product not found")
	}
	// Persist an in-progress marker before contacting Telegram. A restart can safely
	// retry delivery_pending; digital delivery may be repeated, but is never lost.
	o.Status = "delivery_pending"
	a.store.data.Orders[o.ID] = o
	if err := a.store.saveLocked(); err != nil {
		a.store.mu.Unlock()
		return err
	}
	payloads := []string{}
	for _, sid := range o.StockIDs {
		x := a.store.data.Stock[sid]
		if x.OrderID != o.ID {
			a.store.mu.Unlock()
			return errors.New("reserved stock is unavailable")
		}
		payloads = append(payloads, x.Payload)
	}
	a.store.mu.Unlock()
	delivery := fmt.Sprintf("✅ <b>Order delivered</b>\n\nOrder: <code>%s</code>\nItems purchased:\n• %s × %d\n\n<b>Delivery instructions</b>\n%s\n\n<b>Your item(s)</b>\n<pre>%s</pre>", esc(o.ID), esc(prod.Name), o.Quantity, esc(prod.DeliveryInstruction), esc(strings.Join(payloads, "\n\n")))
	if err := a.tg.send(ctx, o.BuyerID, delivery, nil); err != nil {
		a.store.mu.Lock()
		if current, ok := a.store.data.Orders[o.ID]; ok && current.Status == "delivery_pending" {
			current.Status = "payment_submitted"
			a.store.data.Orders[o.ID] = current
			_ = a.store.saveLocked()
		}
		a.store.mu.Unlock()
		return fmt.Errorf("delivery message: %w", err)
	}
	a.store.mu.Lock()
	current, ok := a.store.data.Orders[o.ID]
	if !ok || current.Status != "delivery_pending" {
		a.store.mu.Unlock()
		return errors.New("delivery state changed")
	}
	for _, sid := range current.StockIDs {
		x := a.store.data.Stock[sid]
		x.Sold = true
		a.store.data.Stock[sid] = x
	}
	current.Status = "delivered"
	current.PaidAt = time.Now().UTC()
	a.store.data.Orders[current.ID] = current
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		return err
	}
	a.broadcast(ctx, fmt.Sprintf("✅ <b>New purchase</b>\n\n%s was purchased. Thank you for shopping with us!", esc(prod.Name)), true)
	return nil
}

func (a *App) verifyOrder(ctx context.Context, orderID string) {
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	a.store.mu.Unlock()
	if !ok || (o.Status != "payment_submitted" && o.Status != "delivery_pending") {
		return
	}
	if o.Status == "delivery_pending" {
		if err := a.deliverOrder(ctx, orderID); err != nil {
			log.Printf("retry delivery %s: %v", orderID, err)
		}
		return
	}
	verified, reason, err := a.checkPayment(ctx, o)
	if err != nil {
		log.Printf("verify %s: %v", orderID, err)
		return
	}
	if !verified {
		log.Printf("verify %s pending: %s", orderID, reason)
		return
	}
	if err := a.deliverOrder(ctx, orderID); err != nil {
		log.Printf("auto-deliver %s: %v", orderID, err)
		return
	}
	log.Printf("payment verified and order delivered: %s", orderID)
}

func (a *App) verifyLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.store.mu.Lock()
			ids := []string{}
			for id, o := range a.store.data.Orders {
				if o.Status == "payment_submitted" || o.Status == "delivery_pending" {
					ids = append(ids, id)
				}
			}
			a.store.mu.Unlock()
			for _, id := range ids {
				a.verifyOrder(ctx, id)
			}
		}
	}
}
func (a *App) reject(ctx context.Context, m *Message, p []string) {
	if len(p) < 3 {
		a.tg.send(ctx, m.Chat.ID, "Usage: <code>/reject ORDER_ID reason</code>", nil)
		return
	}
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[p[1]]
	if !ok || o.Status != "payment_submitted" {
		a.store.mu.Unlock()
		a.tg.send(ctx, m.Chat.ID, "Order is not awaiting verification.", nil)
		return
	}
	for _, sid := range o.StockIDs {
		x := a.store.data.Stock[sid]
		x.OrderID = ""
		a.store.data.Stock[sid] = x
	}
	o.Status = "payment_rejected"
	a.store.data.Orders[o.ID] = o
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err == nil {
		a.tg.send(ctx, o.BuyerID, "Payment verification was rejected: "+esc(strings.Join(p[2:], " "))+"\n\nYour stock reservation was released. Contact support if this is unexpected.", nil)
	}
	a.tg.send(ctx, m.Chat.ID, "Order rejected and stock released.", nil)
}
func (a *App) handleCallback(ctx context.Context, c *Callback) {
	a.tg.answer(ctx, c.ID)
	a.remember(&c.From)
	chat := c.From.ID
	if c.Message != nil {
		chat = c.Message.Chat.ID
	}
	switch {
	case c.Data == "catalog":
		t, k := a.productsText()
		a.tg.send(ctx, chat, t, k)
	case strings.HasPrefix(c.Data, "product:"):
		t, k := a.productText(strings.TrimPrefix(c.Data, "product:"))
		a.tg.send(ctx, chat, t, k)
	case strings.HasPrefix(c.Data, "buy:"):
		code := strings.TrimPrefix(c.Data, "buy:")
		a.tg.send(ctx, chat, "Choose the USDT network. Send only on the selected network.", &Markup{InlineKeyboard: [][]Button{{{Text: "BEP-20 (BNB Smart Chain)", Data: "network:bep20:" + code}}, {{Text: "Polygon", Data: "network:polygon:" + code}}}})
	case strings.HasPrefix(c.Data, "network:"):
		p := strings.Split(c.Data, ":")
		if len(p) != 3 {
			return
		}
		o, err := a.createOrder(&c.From, p[2], p[1])
		if err != nil {
			a.tg.send(ctx, chat, "Could not create order: "+esc(err.Error()), nil)
			return
		}
		a.tg.send(ctx, chat, a.paymentText(o), nil)
	}
}
func (a *App) run(ctx context.Context) error {
	go a.verifyLoop(ctx)
	var offset int64
	for {
		var updates []Update
		err := a.tg.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 45, "allowed_updates": []string{"message", "callback_query"}}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("poll: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message != nil {
				a.handleMessage(ctx, u.Message)
			}
			if u.Callback != nil {
				a.handleCallback(ctx, u.Callback)
			}
		}
	}
}
func main() {
	cfg, err := config()
	if err != nil {
		log.Fatal(err)
	}
	s, err := openStore(cfg.DataFile)
	if err != nil {
		log.Fatal(err)
	}
	app := &App{cfg: cfg, store: s, tg: &TG{token: cfg.Token, client: &http.Client{Timeout: 55 * time.Second}}}
	log.Printf("Doraemon Shop Bot started; owner=%d", cfg.OwnerID)
	if err := app.run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
