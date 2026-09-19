package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func signedInitData(t *testing.T, token string, user User, authTime time.Time) string {
	t.Helper()
	userJSON, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{"auth_date": {strconv.FormatInt(authTime.Unix(), 10)}, "query_id": {"test-query"}, "user": {string(userJSON)}}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+values.Get(key))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secret.Write([]byte(token))
	signature := hmac.New(sha256.New, secret.Sum(nil))
	_, _ = signature.Write([]byte(strings.Join(lines, "\n")))
	values.Set("hash", hex.EncodeToString(signature.Sum(nil)))
	return values.Encode()
}

func TestValidateTelegramInitData(t *testing.T) {
	now := time.Now().UTC()
	raw := signedInitData(t, "test-token", User{ID: 42, FirstName: "Buyer"}, now)
	user, err := validateTelegramInitData(raw, "test-token", now)
	if err != nil || user.ID != 42 {
		t.Fatalf("valid launch data rejected: user=%#v error=%v", user, err)
	}
	if _, err := validateTelegramInitData(raw, "different-token", now); err == nil {
		t.Fatal("launch data signed with another token must be rejected")
	}
	old := signedInitData(t, "test-token", User{ID: 42}, now.Add(-miniAppAuthLifetime-time.Second))
	if _, err := validateTelegramInitData(old, "test-token", now); err == nil {
		t.Fatal("expired launch data must be rejected")
	}
}

func TestValidateMiniAppURL(t *testing.T) {
	for _, valid := range []string{"https://shop.example.com", "http://localhost:8080"} {
		if err := validateMiniAppURL(valid); err != nil {
			t.Fatalf("valid URL %q rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{"http://shop.example.com", "javascript:alert(1)", "https:///missing-host", "https://shop.example.com/?token=secret"} {
		if err := validateMiniAppURL(invalid); err == nil {
			t.Fatalf("invalid URL %q accepted", invalid)
		}
	}
}

func TestMiniAppCatalogRequiresSignedTelegramUser(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.data.Products["KEY"] = Product{SKU: "KEY", Name: "Access key", Description: "One key", PriceUSDT: 4.5, Active: true}
	store.data.Stock["stock-1"] = StockItem{ID: "stock-1", SKU: "KEY", Payload: "must-not-leak"}
	app := &App{cfg: Config{Token: "test-token"}, store: store}

	request := httptest.NewRequest(http.MethodGet, "/api/mini-app/catalog", nil)
	request.Header.Set("Authorization", "tma "+signedInitData(t, "test-token", User{ID: 42}, time.Now()))
	response := httptest.NewRecorder()
	app.miniAppCatalog(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "must-not-leak") {
		t.Fatal("private stock payload leaked through catalog API")
	}

	request = httptest.NewRequest(http.MethodGet, "/api/mini-app/catalog", nil)
	response = httptest.NewRecorder()
	app.miniAppCatalog(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned request status=%d", response.Code)
	}
}

func miniAppOwnerRequest(t *testing.T, method, target, token string, owner int64, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "tma "+signedInitData(t, token, User{ID: owner, FirstName: "Owner"}, time.Now()))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestMiniAppAdminRequiresOwnerAndManagesPrivateStock(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{Token: "test-token", OwnerID: 7}, store: store}

	nonOwner := httptest.NewRequest(http.MethodGet, "/api/mini-app/admin/products", nil)
	nonOwner.Header.Set("Authorization", "tma "+signedInitData(t, "test-token", User{ID: 8}, time.Now()))
	denied := httptest.NewRecorder()
	app.miniAppAdminProducts(denied, nonOwner)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("non-owner admin status=%d body=%s", denied.Code, denied.Body.String())
	}

	create := httptest.NewRecorder()
	app.miniAppAdminProducts(create, miniAppOwnerRequest(t, http.MethodPost, "/api/mini-app/admin/products", "test-token", 7, `{"sku":"KEY-1","name":"Access key","priceUsdt":4.5,"description":"One private key","deliveryInstruction":"Redeem the key"}`))
	if create.Code != http.StatusCreated || store.data.Products["KEY-1"].PriceUSDT != 4.5 {
		t.Fatalf("product create status=%d body=%s product=%+v", create.Code, create.Body.String(), store.data.Products["KEY-1"])
	}

	update := httptest.NewRecorder()
	app.miniAppAdminProductItem(update, miniAppOwnerRequest(t, http.MethodPatch, "/api/mini-app/admin/products/KEY-1", "test-token", 7, `{"priceUsdt":9.99}`))
	if update.Code != http.StatusOK || store.data.Products["KEY-1"].PriceUSDT != 9.99 {
		t.Fatalf("price update status=%d body=%s product=%+v", update.Code, update.Body.String(), store.data.Products["KEY-1"])
	}

	add := httptest.NewRecorder()
	app.miniAppAdminStock(add, miniAppOwnerRequest(t, http.MethodPost, "/api/mini-app/admin/stock", "test-token", 7, `{"sku":"KEY-1","payloads":"secret-one\nsecret-two"}`))
	if add.Code != http.StatusCreated || len(store.data.Stock) != 2 {
		t.Fatalf("stock add status=%d body=%s stock=%d", add.Code, add.Body.String(), len(store.data.Stock))
	}

	list := httptest.NewRecorder()
	app.miniAppAdminStock(list, miniAppOwnerRequest(t, http.MethodGet, "/api/mini-app/admin/stock?sku=KEY-1", "test-token", 7, ""))
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "secret-one") || strings.Contains(list.Body.String(), "secret-two") {
		t.Fatalf("admin stock list leaked payload or failed: status=%d body=%s", list.Code, list.Body.String())
	}

	var stockID string
	for id := range store.data.Stock {
		stockID = id
		break
	}
	remove := httptest.NewRecorder()
	app.miniAppAdminStockItem(remove, miniAppOwnerRequest(t, http.MethodDelete, "/api/mini-app/admin/stock/"+stockID, "test-token", 7, ""))
	if remove.Code != http.StatusNoContent || len(store.data.Stock) != 1 {
		t.Fatalf("stock removal status=%d stock=%d", remove.Code, len(store.data.Stock))
	}

	store.data.Orders["active-order"] = Order{ID: "active-order", SKU: "KEY-1", Status: "payment_submitted"}
	blocked := httptest.NewRecorder()
	app.miniAppAdminProductItem(blocked, miniAppOwnerRequest(t, http.MethodDelete, "/api/mini-app/admin/products/KEY-1", "test-token", 7, ""))
	if blocked.Code != http.StatusConflict || store.data.Products["KEY-1"].SKU != "KEY-1" {
		t.Fatalf("active product deletion status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	delete(store.data.Orders, "active-order")
	deleteProduct := httptest.NewRecorder()
	app.miniAppAdminProductItem(deleteProduct, miniAppOwnerRequest(t, http.MethodDelete, "/api/mini-app/admin/products/KEY-1", "test-token", 7, ""))
	if deleteProduct.Code != http.StatusNoContent || len(store.data.Products) != 0 || len(store.data.Stock) != 0 {
		t.Fatalf("product deletion status=%d products=%d stock=%d", deleteProduct.Code, len(store.data.Products), len(store.data.Stock))
	}
}

func TestMiniAppAdminOrdersAreOwnerOnlyAndExcludeDeliverySecrets(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-time.Hour)
	paid := created.Add(4 * time.Minute)
	store.data.Orders["ord-1"] = Order{
		ID: "ord-1", SKU: "GEMINI-18M", ProductName: "Gemini access", Quantity: 2, Amount: 18.5,
		BuyerID: 42, BuyerName: "@buyer", Network: "polygon", TxHash: "0xabc", Status: "delivered",
		PaymentIssue: "", DeliveryInstruction: "private redemption link", CreatedAt: created, PaidAt: paid,
	}
	app := &App{cfg: Config{Token: "test-token", OwnerID: 7}, store: store}

	denied := httptest.NewRecorder()
	app.miniAppAdminOrders(denied, miniAppOwnerRequest(t, http.MethodGet, "/api/mini-app/admin/orders", "test-token", 8, ""))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("non-owner admin orders status=%d body=%s", denied.Code, denied.Body.String())
	}

	response := httptest.NewRecorder()
	app.miniAppAdminOrders(response, miniAppOwnerRequest(t, http.MethodGet, "/api/mini-app/admin/orders", "test-token", 7, ""))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "@buyer") || !strings.Contains(body, "Wallet balance") && !strings.Contains(body, "USDT · Polygon") || !strings.Contains(body, "0xabc") {
		t.Fatalf("owner admin orders status=%d body=%s", response.Code, body)
	}
	if strings.Contains(body, "private redemption link") {
		t.Fatalf("admin order API leaked delivery instructions: %s", body)
	}
}

func TestMiniAppServesIndexForPublicURLPath(t *testing.T) {
	app := &App{cfg: Config{MiniAppURL: "https://shop.example.com/shop/"}}
	handler, err := app.miniAppHandler()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/shop/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ZenitsuThunder Shop") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/shop/styles.css", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), ".catalog") {
		t.Fatalf("prefixed stylesheet status=%d body=%q", response.Code, response.Body.String())
	}
}
