package main

import (
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

func TestMiniAppServesIndexForPublicURLPath(t *testing.T) {
	app := &App{cfg: Config{MiniAppURL: "https://shop.example.com/shop/"}}
	handler, err := app.miniAppHandler()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/shop/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Doraemon Shop") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/shop/styles.css", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), ".catalog") {
		t.Fatalf("prefixed stylesheet status=%d body=%q", response.Code, response.Body.String())
	}
}
