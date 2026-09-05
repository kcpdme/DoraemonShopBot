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
	"net/http"
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
	BEP20USDT      string
	PolygonUSDT    string
	BSCRPCURL      string
	PolygonRPCURL  string
	DataFile       string
}

func config() (Config, error) {
	owner, err := strconv.ParseInt(os.Getenv("OWNER_TELEGRAM_ID"), 10, 64)
	if err != nil || owner == 0 {
		return Config{}, errors.New("OWNER_TELEGRAM_ID must be a numeric Telegram user ID")
	}
	c := Config{Token: os.Getenv("BOT_TOKEN"), OwnerID: owner, Channel: os.Getenv("SALES_CHANNEL"), BEP20Address: os.Getenv("BEP20_USDT_ADDRESS"), PolygonAddress: os.Getenv("POLYGON_USDT_ADDRESS"), BEP20USDT: os.Getenv("BEP20_USDT_CONTRACT"), PolygonUSDT: os.Getenv("POLYGON_USDT_CONTRACT"), BSCRPCURL: os.Getenv("BSC_RPC_URL"), PolygonRPCURL: os.Getenv("POLYGON_RPC_URL"), DataFile: os.Getenv("DATA_FILE")}
	if c.Token == "" || c.BEP20Address == "" || c.PolygonAddress == "" {
		return Config{}, errors.New("BOT_TOKEN, BEP20_USDT_ADDRESS and POLYGON_USDT_ADDRESS are required")
	}
	if c.BEP20USDT == "" || c.PolygonUSDT == "" {
		return Config{}, errors.New("BEP20_USDT_CONTRACT and POLYGON_USDT_CONTRACT are required for automatic verification")
	}
	for name, value := range map[string]string{"BEP20_USDT_ADDRESS": c.BEP20Address, "POLYGON_USDT_ADDRESS": c.PolygonAddress, "BEP20_USDT_CONTRACT": c.BEP20USDT, "POLYGON_USDT_CONTRACT": c.PolygonUSDT} {
		if !validHexBytes(value, 20) {
			return Config{}, fmt.Errorf("%s must be a complete 20-byte EVM address", name)
		}
	}
	if c.DataFile == "" {
		c.DataFile = "data/store.json"
	}
	if c.Channel != "" && !strings.HasPrefix(c.Channel, "@") && !strings.HasPrefix(c.Channel, "-") {
		c.Channel = "@" + c.Channel
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
	ProductName, DeliveryInstruction, PaymentIssue string
	PaymentMessageID                               int64
	DeliveryPartsSent                              int
	ID, SKU                                        string
	Quantity                                       int
	BuyerID                                        int64
	BuyerName                                      string
	Amount                                         float64
	Network, TxHash, Status                        string
	StockIDs                                       []string
	CreatedAt, PaidAt                              time.Time
}
type StoreData struct {
	Products map[string]Product
	Stock    map[string]StockItem
	Orders   map[string]Order
	Started  map[int64]string
}
type Store struct {
	mu        sync.Mutex
	path      string
	data      StoreData
	committed []byte
}

func openStore(path string) (*Store, error) {
	s := &Store{path: path, data: StoreData{Products: map[string]Product{}, Stock: map[string]StockItem{}, Orders: map[string]Order{}, Started: map[int64]string{}}}
	s.committed, _ = json.Marshal(s.data)
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
	s.committed, _ = json.Marshal(s.data)
	return s, nil
}
func (s *Store) saveLocked() (saveErr error) {
	defer func() {
		if saveErr != nil && len(s.committed) > 0 {
			var old StoreData
			if json.Unmarshal(s.committed, &old) == nil {
				s.data = old
			}
		}
	}()
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
	if err = os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.committed = b
	return nil
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
	MessageID int64    `json:"message_id"`
	Chat      Chat     `json:"chat"`
	From      *User    `json:"from"`
	Text      string   `json:"text"`
	ReplyTo   *Message `json:"reply_to_message"`
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
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
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
func (t *TG) configureCommands(ctx context.Context) error {
	return t.call(ctx, "setMyCommands", map[string]any{"commands": []map[string]string{
		{"command": "start", "description": "Open the shop menu"},
		{"command": "shop", "description": "Browse available products"},
		{"command": "orders", "description": "View your orders"},
		{"command": "support", "description": "Contact support"},
		{"command": "help", "description": "How to use the bot"},
	}}, nil)
}

type App struct {
	verifying sync.Map   // One in-flight RPC check per order, without blocking checkout.
	opMu      sync.Mutex // Serializes proof, delivery, cancellation and expiration.
	cfg       Config
	store     *Store
	tg        *TG
	flows     sync.Map // chat ID -> in-chat wizard/support state (deliberately ephemeral)
}

type flow struct {
	Kind, SKU, Name, Description, Instruction string
	Price                                     float64
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

func (a *App) home(ctx context.Context, chat, userID int64) {
	rows := [][]Button{{{Text: "🛍 Shop", Data: "catalog"}, {Text: "📦 My orders", Data: "orders"}}, {{Text: "💬 Support", Data: "support"}}}
	if admin(a, userID) {
		rows = append(rows, []Button{{Text: "⚙️ Owner panel", Data: "admin:panel"}})
	}
	a.tg.send(ctx, chat, "🏪 <b>Doraemon Shop</b>\n\nDigital goods with automatic USDT payment verification and private delivery.\n\nChoose an option below.", &Markup{InlineKeyboard: rows})
}

func (a *App) channelURL() string {
	name := strings.TrimPrefix(strings.TrimSpace(a.cfg.Channel), "@")
	if name == "" || strings.HasPrefix(name, "-") {
		return ""
	}
	return "https://t.me/" + name
}

func (a *App) requireChannel(ctx context.Context, chat, userID int64) bool {
	if admin(a, userID) || a.cfg.Channel == "" {
		return true
	}
	var member struct {
		Status   string `json:"status"`
		IsMember bool   `json:"is_member"`
	}
	err := a.tg.call(ctx, "getChatMember", map[string]any{"chat_id": a.cfg.Channel, "user_id": userID}, &member)
	joined := err == nil && (member.Status == "creator" || member.Status == "administrator" || member.Status == "member" || (member.Status == "restricted" && member.IsMember))
	if joined {
		return true
	}
	url := a.channelURL()
	rows := [][]Button{}
	if url != "" {
		rows = append(rows, []Button{{Text: "📢 Join updates channel", URL: url}})
	}
	rows = append(rows, []Button{{Text: "✅ I joined — continue", Data: "gate:check"}})
	msg := "🔐 <b>Join required</b>\n\nJoin our updates channel before browsing the shop, then tap <b>I joined — continue</b>."
	if err != nil {
		msg += "\n\n<i>Membership verification is temporarily unavailable. The bot must be a channel administrator.</i>"
		log.Printf("channel gate: %v", err)
	}
	a.tg.send(ctx, chat, msg, &Markup{InlineKeyboard: rows})
	return false
}

func (a *App) setFlow(chat int64, f flow) { a.flows.Store(chat, f) }
func (a *App) clearFlow(chat int64)       { a.flows.Delete(chat) }
func (a *App) flowFor(chat int64) (flow, bool) {
	v, ok := a.flows.Load(chat)
	if !ok {
		return flow{}, false
	}
	f, ok := v.(flow)
	return f, ok
}

func (a *App) adminPanel(ctx context.Context, chat int64) {
	a.tg.send(ctx, chat, "⚙️ <b>Owner panel</b>\n\nManage catalog, inventory, orders, support, and announcements.", &Markup{InlineKeyboard: [][]Button{
		{{Text: "➕ New product", Data: "admin:newproduct"}, {Text: "📦 Add stock", Data: "admin:stock"}},
		{{Text: "🧾 Orders", Data: "admin:orders"}, {Text: "🛍 Products", Data: "admin:products"}},
		{{Text: "📣 Broadcast", Data: "admin:broadcast"}, {Text: "📊 Dashboard", Data: "admin:dashboard"}},
		{{Text: "🏠 Customer menu", Data: "home"}},
	}})
}

func (a *App) dashboard(ctx context.Context, chat int64) {
	a.store.mu.Lock()
	products, available, pending, delivered := 0, 0, 0, 0
	for _, p := range a.store.data.Products {
		if p.Active {
			products++
		}
	}
	for _, x := range a.store.data.Stock {
		if !x.Sold && x.OrderID == "" {
			available++
		}
	}
	for _, o := range a.store.data.Orders {
		if o.Status == "awaiting_payment" || o.Status == "payment_submitted" || o.Status == "delivery_pending" {
			pending++
		}
		if o.Status == "delivered" {
			delivered++
		}
	}
	users := len(a.store.data.Started)
	a.store.mu.Unlock()
	a.tg.send(ctx, chat, fmt.Sprintf("📊 <b>Store dashboard</b>\n\nActive products: <b>%d</b>\nAvailable stock units: <b>%d</b>\nOrders awaiting action: <b>%d</b>\nDelivered orders: <b>%d</b>\nStarted users: <b>%d</b>", products, available, pending, delivered, users), &Markup{InlineKeyboard: [][]Button{{{Text: "← Owner panel", Data: "admin:panel"}}}})
}

func (a *App) handleFlow(ctx context.Context, m *Message, text string) bool {
	f, ok := a.flowFor(m.Chat.ID)
	if !ok {
		return false
	}
	if strings.HasPrefix(text, "/") {
		if text == "/cancel" {
			a.clearFlow(m.Chat.ID)
			a.tg.send(ctx, m.Chat.ID, "Cancelled.", nil)
			return true
		}
		return false
	}
	switch f.Kind {
	case "quantity":
		quantity, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			_ = a.tg.send(ctx, m.Chat.ID, "Enter a whole number between 1 and 50.", nil)
			return true
		}
		if err = a.showNetworkChoice(ctx, m.Chat.ID, f.SKU, quantity); err != nil {
			_ = a.tg.send(ctx, m.Chat.ID, esc(err.Error()), nil)
		} else {
			a.clearFlow(m.Chat.ID)
		}
		return true
	case "proof":
		if text == "" {
			return true
		}
		a.submitTxHash(ctx, m.Chat.ID, m.From.ID, f.SKU, text)
		return true
	case "support":
		if text == "" {
			return true
		}
		a.clearFlow(m.Chat.ID)
		a.tg.send(ctx, m.Chat.ID, "✅ Your message was sent to support. We’ll reply here.", nil)
		a.tg.send(ctx, a.cfg.OwnerID, fmt.Sprintf("💬 <b>Support request</b>\nFrom: %s\n\n%s\n\n↩️ <i>Reply directly to this message to answer as the bot.</i>\n#support:%d", esc(userLabel(m.From)), esc(text), m.Chat.ID), nil)
		return true
	case "broadcast":
		if !admin(a, m.From.ID) {
			a.clearFlow(m.Chat.ID)
			return true
		}
		if text == "" {
			return true
		}
		a.clearFlow(m.Chat.ID)
		a.broadcast(ctx, esc(text), true)
		a.tg.send(ctx, m.Chat.ID, "✅ Broadcast sent to started users and the sales channel.", nil)
		return true
	case "stock":
		if !admin(a, m.From.ID) {
			a.clearFlow(m.Chat.ID)
			return true
		}
		if text == "" {
			return true
		}
		a.clearFlow(m.Chat.ID)
		a.addStockPayload(ctx, m.Chat.ID, f.SKU, text)
		return true
	case "new_sku":
		if !admin(a, m.From.ID) {
			a.clearFlow(m.Chat.ID)
			return true
		}
		f.SKU = sku(text)
		if f.SKU == "" {
			a.tg.send(ctx, m.Chat.ID, "Use letters, numbers, or hyphens for the SKU.", nil)
			return true
		}
		f.Kind = "new_name"
		a.setFlow(m.Chat.ID, f)
		a.tg.send(ctx, m.Chat.ID, "Product name?", nil)
		return true
	case "new_name":
		f.Name = strings.TrimSpace(text)
		if f.Name == "" {
			a.tg.send(ctx, m.Chat.ID, "Enter a product name.", nil)
			return true
		}
		f.Kind = "new_price"
		a.setFlow(m.Chat.ID, f)
		a.tg.send(ctx, m.Chat.ID, "Price in USDT? Example: <code>12.50</code>", nil)
		return true
	case "new_price":
		price, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil || price <= 0 {
			a.tg.send(ctx, m.Chat.ID, "Enter a positive USDT price, for example <code>12.50</code>.", nil)
			return true
		}
		f.Price = price
		f.Kind = "new_description"
		a.setFlow(m.Chat.ID, f)
		a.tg.send(ctx, m.Chat.ID, "Product description shown before payment?", nil)
		return true
	case "new_description":
		f.Description = strings.TrimSpace(text)
		if f.Description == "" {
			a.tg.send(ctx, m.Chat.ID, "Enter a description.", nil)
			return true
		}
		f.Kind = "new_instruction"
		a.setFlow(m.Chat.ID, f)
		a.tg.send(ctx, m.Chat.ID, "Delivery instruction shown after payment? Example: login steps, redemption instructions, or support policy.", nil)
		return true
	case "new_instruction":
		f.Instruction = strings.TrimSpace(text)
		if f.Instruction == "" {
			a.tg.send(ctx, m.Chat.ID, "Enter delivery instructions.", nil)
			return true
		}
		a.clearFlow(m.Chat.ID)
		a.saveProduct(ctx, m.Chat.ID, f)
		return true
	}
	return false
}

func (a *App) saveProduct(ctx context.Context, chat int64, f flow) {
	a.store.mu.Lock()
	_, exists := a.store.data.Products[f.SKU]
	a.store.data.Products[f.SKU] = Product{SKU: f.SKU, Name: f.Name, PriceUSDT: f.Price, Description: f.Description, DeliveryInstruction: f.Instruction, Active: true, CreatedAt: time.Now().UTC()}
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		a.tg.send(ctx, chat, "Could not save the product.", nil)
		return
	}
	action := "created"
	if exists {
		action = "updated"
	}
	a.tg.send(ctx, chat, fmt.Sprintf("✅ Product <b>%s</b> %s. Add delivery units with <b>Add stock</b>.", esc(f.Name), action), &Markup{InlineKeyboard: [][]Button{{{Text: "📦 Add stock", Data: "admin:stocksku:" + f.SKU}, {Text: "⚙️ Owner panel", Data: "admin:panel"}}}})
}

func (a *App) handleMessage(ctx context.Context, m *Message) {
	if m == nil || m.From == nil {
		return
	}
	if m.Chat.ID != m.From.ID {
		return
	} // Payments, stock and support are private-chat only.
	a.remember(m.From)
	text := strings.TrimSpace(m.Text)
	if admin(a, m.From.ID) && m.ReplyTo != nil {
		if customerChat, ok := supportReplyTarget(m.ReplyTo.Text); ok {
			a.forwardSupportReply(ctx, m.Chat.ID, customerChat, text)
			return
		}
	}
	if a.handleFlow(ctx, m, text) {
		return
	}
	parts := strings.Fields(text)
	cmd := ""
	if len(parts) > 0 {
		cmd = strings.Split(parts[0], "@")[0]
	}
	if !admin(a, m.From.ID) && (cmd == "/start" || cmd == "/shop") && !a.requireChannel(ctx, m.Chat.ID, m.From.ID) {
		return
	}
	switch cmd {
	case "/start":
		a.home(ctx, m.Chat.ID, m.From.ID)
	case "/shop":
		t, k := a.productsText()
		a.tg.send(ctx, m.Chat.ID, t, k)
	case "/help":
		a.tg.send(ctx, m.Chat.ID, "/shop — browse products\n/orders — view orders\n/paid ORDER_ID TX_HASH — submit payment\n/support — contact support\n/cancel — cancel an active step", nil)
	case "/orders":
		a.sendOrders(ctx, m.Chat.ID, m.From.ID)
	case "/paid":
		a.submitPaid(ctx, m, parts)
	case "/admin":
		if admin(a, m.From.ID) {
			a.adminPanel(ctx, m.Chat.ID)
		}
	case "/support":
		a.setFlow(m.Chat.ID, flow{Kind: "support"})
		a.tg.send(ctx, m.Chat.ID, "💬 Send one message describing your issue. Include an order ID if relevant.\n\nUse /cancel to stop.", nil)
	case "/reply":
		if admin(a, m.From.ID) {
			a.replySupport(ctx, m, parts)
		}
	case "/cancel":
		a.clearFlow(m.Chat.ID)
		a.tg.send(ctx, m.Chat.ID, "Cancelled.", nil)
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
	a.sendOrdersPage(ctx, chat, buyer, 0)
}

func (a *App) sendOrdersPage(ctx context.Context, chat, buyer int64, page int) {
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
	rows := [][]Button{}
	if page < 0 || page > (len(os)-1)/8 {
		page = 0
	}
	end := min(len(os), page*8+8)
	for _, o := range os[page*8 : end] {
		b.WriteString(fmt.Sprintf("<code>%s</code> · %s · %.2f USDT · <b>%s</b>\n", o.ID, esc(o.SKU), o.Amount, esc(friendlyOrderStatus(o.Status))))
		rows = append(rows, []Button{{Text: "View " + o.ID, Data: "order:" + o.ID}})
	}
	nav := []Button{}
	if page > 0 {
		nav = append(nav, Button{Text: "‹ Newer", Data: fmt.Sprintf("orders:%d", page-1)})
	}
	if end < len(os) {
		nav = append(nav, Button{Text: "Older ›", Data: fmt.Sprintf("orders:%d", page+1)})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []Button{{Text: "🛍 Shop", Data: "catalog"}, {Text: "💬 Support", Data: "support"}})
	a.tg.send(ctx, chat, b.String(), &Markup{InlineKeyboard: rows})
}

func (a *App) orderDetail(ctx context.Context, chat, buyer int64, orderID string) {
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	a.store.mu.Unlock()
	if !ok || o.BuyerID != buyer {
		a.tg.send(ctx, chat, "Order not found.", nil)
		return
	}
	_ = a.sendPaymentCard(ctx, chat, o, 0)
}

func (a *App) cancelOrder(ctx context.Context, chat, buyer int64, orderID string) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	if !ok || o.BuyerID != buyer || (o.Status != "awaiting_payment" && o.Status != "proof_invalid") {
		a.store.mu.Unlock()
		a.tg.send(ctx, chat, "Only unpaid orders can be cancelled.", nil)
		return
	}
	for _, sid := range o.StockIDs {
		x := a.store.data.Stock[sid]
		x.OrderID = ""
		a.store.data.Stock[sid] = x
	}
	o.Status = "cancelled"
	a.store.data.Orders[o.ID] = o
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		a.tg.send(ctx, chat, "Could not cancel order.", nil)
		return
	}
	_ = a.sendPaymentCard(ctx, chat, o, o.PaymentMessageID)
}

func (a *App) stockCount(sku string) int {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	n := 0
	for _, x := range a.store.data.Stock {
		if x.SKU == sku && !x.Sold && x.OrderID == "" {
			n++
		}
	}
	return n
}

func (a *App) adminProducts(ctx context.Context, chat int64) {
	a.store.mu.Lock()
	ps := []Product{}
	for _, p := range a.store.data.Products {
		ps = append(ps, p)
	}
	a.store.mu.Unlock()
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
	if len(ps) == 0 {
		a.tg.send(ctx, chat, "No products yet. Use <b>New product</b> to create one.", &Markup{InlineKeyboard: [][]Button{{{Text: "➕ New product", Data: "admin:newproduct"}, {Text: "← Owner panel", Data: "admin:panel"}}}})
		return
	}
	var b strings.Builder
	b.WriteString("🛍 <b>Products</b>\n\n")
	rows := [][]Button{}
	for _, p := range ps {
		state := "active"
		if !p.Active {
			state = "hidden"
		}
		b.WriteString(fmt.Sprintf("<b>%s</b> · %s\n%s · %.2f USDT · %d in stock\n\n", esc(p.Name), state, esc(p.SKU), p.PriceUSDT, a.stockCount(p.SKU)))
		rows = append(rows, []Button{{Text: "Toggle " + p.SKU, Data: "admin:toggle:" + p.SKU}, {Text: "Add stock", Data: "admin:stocksku:" + p.SKU}})
	}
	rows = append(rows, []Button{{Text: "➕ New product", Data: "admin:newproduct"}, {Text: "← Owner panel", Data: "admin:panel"}})
	a.tg.send(ctx, chat, b.String(), &Markup{InlineKeyboard: rows})
}

func (a *App) adminStockMenu(ctx context.Context, chat int64) {
	a.store.mu.Lock()
	ps := []Product{}
	for _, p := range a.store.data.Products {
		if p.Active {
			ps = append(ps, p)
		}
	}
	a.store.mu.Unlock()
	rows := [][]Button{}
	for _, p := range ps {
		rows = append(rows, []Button{{Text: fmt.Sprintf("%s (%d left)", p.Name, a.stockCount(p.SKU)), Data: "admin:stocksku:" + p.SKU}})
	}
	rows = append(rows, []Button{{Text: "← Owner panel", Data: "admin:panel"}})
	a.tg.send(ctx, chat, "📦 <b>Add stock</b>\n\nChoose a product. You can paste several stock items at once — one item per line.", &Markup{InlineKeyboard: rows})
}

func (a *App) adminOrders(ctx context.Context, chat int64) {
	a.store.mu.Lock()
	os := []Order{}
	for _, o := range a.store.data.Orders {
		os = append(os, o)
	}
	a.store.mu.Unlock()
	sort.Slice(os, func(i, j int) bool { return os[i].CreatedAt.After(os[j].CreatedAt) })
	if len(os) > 12 {
		os = os[:12]
	}
	if len(os) == 0 {
		a.tg.send(ctx, chat, "No orders yet.", nil)
		return
	}
	rows := [][]Button{}
	for _, o := range os {
		rows = append(rows, []Button{{Text: fmt.Sprintf("%s · %s", o.ID, o.Status), Data: "admin:order:" + o.ID}})
	}
	rows = append(rows, []Button{{Text: "← Owner panel", Data: "admin:panel"}})
	a.tg.send(ctx, chat, "🧾 <b>Recent orders</b>\n\nSelect an order for payment and delivery details.", &Markup{InlineKeyboard: rows})
}

func (a *App) adminOrderDetail(ctx context.Context, chat int64, orderID string) {
	a.store.mu.Lock()
	o, ok := a.store.data.Orders[orderID]
	p := a.store.data.Products[o.SKU]
	a.store.mu.Unlock()
	if !ok {
		a.tg.send(ctx, chat, "Order not found.", nil)
		return
	}
	tx := o.TxHash
	if tx == "" {
		tx = "not submitted"
	}
	text := fmt.Sprintf("🧾 <b>Order %s</b>\n\nBuyer: %s\nProduct: %s\nAmount: %.2f USDT\nNetwork: %s\nStatus: <b>%s</b>\nTx hash: <code>%s</code>", esc(o.ID), esc(o.BuyerName), esc(p.Name), o.Amount, esc(o.Network), esc(o.Status), esc(tx))
	rows := [][]Button{}
	if o.Status == "payment_submitted" || o.Status == "delivery_pending" {
		rows = append(rows, []Button{{Text: "⚡ Deliver now", Data: "admin:deliver:" + o.ID}})
	}
	rows = append(rows, []Button{{Text: "← Orders", Data: "admin:orders"}})
	a.tg.send(ctx, chat, text, &Markup{InlineKeyboard: rows})
}

func (a *App) toggleProduct(ctx context.Context, chat int64, sku string) {
	a.store.mu.Lock()
	p, ok := a.store.data.Products[sku]
	if !ok {
		a.store.mu.Unlock()
		a.tg.send(ctx, chat, "Product not found.", nil)
		return
	}
	p.Active = !p.Active
	a.store.data.Products[sku] = p
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		a.tg.send(ctx, chat, "Could not update product.", nil)
		return
	}
	state := "hidden"
	if p.Active {
		state = "active"
	}
	a.tg.send(ctx, chat, fmt.Sprintf("%s is now <b>%s</b>.", esc(p.Name), state), nil)
}
func (a *App) submitPaid(ctx context.Context, m *Message, p []string) {
	if len(p) != 3 {
		a.tg.send(ctx, m.Chat.ID, "Usage: <code>/paid ORDER_ID TRANSACTION_HASH</code>", nil)
		return
	}
	a.submitTxHash(ctx, m.Chat.ID, m.From.ID, p[1], p[2])
}

func (a *App) adminHelp(ctx context.Context, chat int64) {
	a.adminPanel(ctx, chat)
}

func (a *App) replySupport(ctx context.Context, m *Message, parts []string) {
	if len(parts) < 3 {
		a.tg.send(ctx, m.Chat.ID, "Usage: <code>/reply CUSTOMER_CHAT_ID your message</code>", nil)
		return
	}
	chat, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || chat == 0 {
		a.tg.send(ctx, m.Chat.ID, "Invalid customer chat ID.", nil)
		return
	}
	msg := strings.TrimSpace(strings.Join(parts[2:], " "))
	if msg == "" {
		return
	}
	if err := a.tg.send(ctx, chat, "💬 <b>Support reply</b>\n\n"+esc(msg), nil); err != nil {
		a.tg.send(ctx, m.Chat.ID, "Could not deliver the support reply: "+esc(err.Error()), nil)
		return
	}
	a.tg.send(ctx, m.Chat.ID, "✅ Support reply delivered.", nil)
}

func supportReplyTarget(text string) (int64, bool) {
	marker := "#support:"
	i := strings.LastIndex(text, marker)
	if i < 0 {
		return 0, false
	}
	raw := strings.Fields(strings.TrimSpace(text[i+len(marker):]))
	if len(raw) == 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(raw[0], 10, 64)
	return id, err == nil && id != 0
}

func (a *App) forwardSupportReply(ctx context.Context, ownerChat, customerChat int64, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		a.tg.send(ctx, ownerChat, "Empty support reply ignored.", nil)
		return
	}
	if err := a.tg.send(ctx, customerChat, "💬 <b>Support reply</b>\n\n"+esc(text), nil); err != nil {
		a.tg.send(ctx, ownerChat, "Could not deliver the support reply: "+esc(err.Error()), nil)
		return
	}
	a.tg.send(ctx, ownerChat, "✅ Support reply delivered.", nil)
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
	a.addStockPayload(ctx, m.Chat.ID, sku(x[0]), x[1])
}
func (a *App) addStockPayload(ctx context.Context, chat int64, code, raw string) {
	// One line is one FIFO delivery unit. This makes bulk restocks possible without
	// exposing stock values anywhere except the owner chat and encrypted-at-rest host.
	payloads := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			payloads = append(payloads, p)
		}
	}
	if len(payloads) == 0 {
		a.tg.send(ctx, chat, "Enter at least one non-empty delivery payload.", nil)
		return
	}
	a.store.mu.Lock()
	p, ok := a.store.data.Products[code]
	if !ok {
		a.store.mu.Unlock()
		a.tg.send(ctx, chat, "Unknown SKU.", nil)
		return
	}
	for _, payload := range payloads {
		st := StockItem{ID: id("stock"), SKU: code, Payload: payload, AddedAt: time.Now().UTC()}
		a.store.data.Stock[st.ID] = st
	}
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		a.tg.send(ctx, chat, "Save failed.", nil)
		return
	}
	a.broadcast(ctx, fmt.Sprintf("📦 <b>Restocked</b>\n\n%s is available for <b>%.2f USDT</b>.\nOpen the bot and tap /shop to buy.", esc(p.Name), p.PriceUSDT), true)
	a.tg.send(ctx, chat, fmt.Sprintf("✅ Added <b>%d</b> stock unit(s) for %s and sent the restock announcement.", len(payloads), esc(p.Name)), nil)
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

func (a *App) reject(ctx context.Context, m *Message, p []string) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
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
	if chat != c.From.ID {
		return
	}
	if c.Data == "gate:check" {
		if a.requireChannel(ctx, chat, c.From.ID) {
			a.tg.send(ctx, chat, "✅ Membership verified. Welcome!", nil)
			a.home(ctx, chat, c.From.ID)
		}
		return
	}
	shopping := c.Data == "catalog" || c.Data == "home" || strings.HasPrefix(c.Data, "product:") || strings.HasPrefix(c.Data, "buy:") || strings.HasPrefix(c.Data, "qty:") || strings.HasPrefix(c.Data, "network:")
	if shopping && !admin(a, c.From.ID) && !a.requireChannel(ctx, chat, c.From.ID) {
		return
	}
	a.clearFlow(chat)
	if a.handleCheckoutCallback(ctx, c, chat) {
		return
	}
	switch {
	case c.Data == "home":
		a.home(ctx, chat, c.From.ID)
	case c.Data == "orders":
		a.sendOrders(ctx, chat, c.From.ID)
	case strings.HasPrefix(c.Data, "orders:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(c.Data, "orders:"))
		a.sendOrdersPage(ctx, chat, c.From.ID, page)
	case strings.HasPrefix(c.Data, "order:"):
		a.orderDetail(ctx, chat, c.From.ID, strings.TrimPrefix(c.Data, "order:"))
	case strings.HasPrefix(c.Data, "proof:"):
		a.beginProof(ctx, chat, c.From.ID, strings.TrimPrefix(c.Data, "proof:"))
	case strings.HasPrefix(c.Data, "check:"):
		a.checkOrderNow(ctx, chat, c.From.ID, strings.TrimPrefix(c.Data, "check:"))
	case strings.HasPrefix(c.Data, "cancel:"):
		a.cancelOrder(ctx, chat, c.From.ID, strings.TrimPrefix(c.Data, "cancel:"))
	case c.Data == "support":
		a.setFlow(chat, flow{Kind: "support"})
		a.tg.send(ctx, chat, "💬 Send one message describing your issue. Include an order ID if relevant.\n\nUse /cancel to stop.", nil)
	case c.Data == "admin:panel":
		if admin(a, c.From.ID) {
			a.adminPanel(ctx, chat)
		}
	case c.Data == "admin:dashboard":
		if admin(a, c.From.ID) {
			a.dashboard(ctx, chat)
		}
	case c.Data == "admin:newproduct":
		if admin(a, c.From.ID) {
			a.setFlow(chat, flow{Kind: "new_sku"})
			a.tg.send(ctx, chat, "➕ <b>New product</b>\n\nSend a unique SKU, for example <code>NETFLIX-1M</code>.\n\nUse /cancel to stop.", nil)
		}
	case c.Data == "admin:stock":
		if admin(a, c.From.ID) {
			a.adminStockMenu(ctx, chat)
		}
	case strings.HasPrefix(c.Data, "admin:stocksku:"):
		if admin(a, c.From.ID) {
			code := strings.TrimPrefix(c.Data, "admin:stocksku:")
			a.setFlow(chat, flow{Kind: "stock", SKU: code})
			a.tg.send(ctx, chat, "Send private stock for <code>"+esc(code)+"</code>.\n\nOne line = one delivery unit.\n\nUse /cancel to stop.", nil)
		}
	case c.Data == "admin:products":
		if admin(a, c.From.ID) {
			a.adminProducts(ctx, chat)
		}
	case strings.HasPrefix(c.Data, "admin:toggle:"):
		if admin(a, c.From.ID) {
			a.toggleProduct(ctx, chat, strings.TrimPrefix(c.Data, "admin:toggle:"))
		}
	case c.Data == "admin:orders":
		if admin(a, c.From.ID) {
			a.adminOrders(ctx, chat)
		}
	case strings.HasPrefix(c.Data, "admin:order:"):
		if admin(a, c.From.ID) {
			a.adminOrderDetail(ctx, chat, strings.TrimPrefix(c.Data, "admin:order:"))
		}
	case strings.HasPrefix(c.Data, "admin:deliver:"):
		if admin(a, c.From.ID) {
			if err := a.deliverOrder(ctx, strings.TrimPrefix(c.Data, "admin:deliver:")); err != nil {
				a.tg.send(ctx, chat, "Could not deliver: "+esc(err.Error()), nil)
			} else {
				a.tg.send(ctx, chat, "Delivery sent.", nil)
			}
		}
	case c.Data == "admin:broadcast":
		if admin(a, c.From.ID) {
			a.setFlow(chat, flow{Kind: "broadcast"})
			a.tg.send(ctx, chat, "📣 Send the announcement now. It will go to every user who started the bot and to the sales channel.\n\nUse /cancel to stop.", nil)
		}
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
	if len(os.Args) == 3 && os.Args[1] == "--check-payment" {
		o, exists := s.data.Orders[os.Args[2]]
		if !exists {
			log.Fatal("Order not found")
		}
		ok, reason, err := app.checkPayment(context.Background(), o)
		fmt.Printf("Verified: %t; permanent: %t; reason: %s; error: %v\n", ok, isPermanentPaymentError(err), reason, err)
		if err != nil && !isPermanentPaymentError(err) {
			urls, wallet, token, decimals, chain := rpcList(cfg.BSCRPCURL, defaultBSCRPCs), cfg.BEP20Address, cfg.BEP20USDT, 18, uint64(56)
			if o.Network == "polygon" {
				urls, wallet, token, decimals, chain = rpcList(cfg.PolygonRPCURL, defaultPolygonRPCs), cfg.PolygonAddress, cfg.PolygonUSDT, 6, 137
			}
			for i, endpoint := range urls {
				valid, why, problem := app.checkPaymentAt(context.Background(), endpoint, o, wallet, token, decimals, chain)
				fmt.Printf("Provider %d: verified=%t reason=%s error=%v\n", i+1, valid, why, problem)
			}
		}
		return // Read-only diagnostics: no polling, notifications or data writes.
	}
	if err := app.tg.configureCommands(context.Background()); err != nil {
		log.Printf("set commands: %v", err)
	}
	log.Printf("Doraemon Shop Bot started; owner=%d", cfg.OwnerID)
	if err := app.run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
