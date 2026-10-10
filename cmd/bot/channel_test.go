package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func optionalChannelTG(t *testing.T, messages *[]string) *TG {
	t.Helper()
	return &TG{token: "test-token", client: &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/getChatMember") {
			t.Fatal("optional channel policy made a membership request")
		}
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if messages != nil && payload.Text != "" {
			*messages = append(*messages, payload.Text)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":123}}`)), Header: http.Header{}}, nil
	})}}
}

func TestChannelMembershipOptionalByDefault(t *testing.T) {
	// No Telegram client exists: optional access must work even during an outage.
	a := &App{cfg: Config{Channel: "@updates", OwnerID: 1}}
	if joined, err := a.channelMember(context.Background(), 42); !joined || err != nil {
		t.Fatalf("optional access rejected: joined=%t err=%v", joined, err)
	}
	if !a.requireChannel(context.Background(), 42, 42) {
		t.Fatal("optional chat access rejected")
	}
}

func TestOptionalChannelChatEntryPoints(t *testing.T) {
	for _, command := range []string{"/start", "/shop", "/wallet", "/apikey generate"} {
		t.Run(command, func(t *testing.T) {
			a := checkoutUIFixture(t, 2)
			a.cfg.Channel = "@updates"
			var messages []string
			a.tg = optionalChannelTG(t, &messages)
			a.handleMessage(context.Background(), &Message{From: &User{ID: 42}, Chat: Chat{ID: 42}, Text: command})
			if len(messages) == 0 || strings.Contains(strings.Join(messages, "\n"), "Join required") {
				t.Fatalf("customer did not reach %s: %v", command, messages)
			}
		})
	}
	for _, data := range []string{"home", "catalog", "wallet", "product:DIGITAL", "buy:DIGITAL", "qty:DIGITAL:1", "network:polygon:DIGITAL:1", "gate:check"} {
		t.Run(data, func(t *testing.T) {
			a := checkoutUIFixture(t, 2)
			a.cfg.Channel = "@updates"
			var messages []string
			a.tg = optionalChannelTG(t, &messages)
			a.handleCallback(context.Background(), &Callback{ID: "callback", Data: data, From: User{ID: 42}, Message: &Message{Chat: Chat{ID: 42}}})
			text := strings.Join(messages, "\n")
			if len(messages) == 0 || strings.Contains(text, "Join required") || strings.Contains(text, "Membership verified") {
				t.Fatalf("customer blocked or falsely verified on %s: %v", data, messages)
			}
			if strings.HasPrefix(data, "network:") && len(a.store.data.Orders) != 1 {
				t.Fatal("optional customer could not create a chat order")
			}
		})
	}
}

func TestOptionalChannelMiniAppCatalogAndCheckout(t *testing.T) {
	a := checkoutUIFixture(t, 2)
	a.cfg.Token, a.cfg.Channel = "test-token", "@updates"
	a.tg = optionalChannelTG(t, nil)
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/mini-app/catalog", "", http.StatusOK},
		{"POST", "/api/mini-app/orders", `{"sku":"DIGITAL","quantity":1,"network":"polygon"}`, http.StatusCreated},
	} {
		r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		r.Header.Set("Authorization", "tma "+signedInitData(t, a.cfg.Token, User{ID: 42}, time.Now()))
		w := httptest.NewRecorder()
		if test.method == "GET" {
			a.miniAppCatalog(w, r)
		} else {
			a.miniAppOrders(w, r)
		}
		if w.Code != test.status {
			t.Fatalf("%s %s: status=%d body=%s", test.method, test.path, w.Code, w.Body.String())
		}
	}
}

func TestOptionalChannelResellerPurchase(t *testing.T) {
	a, token, h := resellerFixture(t)
	a.cfg.Channel = "@updates"
	a.tg = optionalChannelTG(t, nil)
	w := resellerRequest(h, "POST", resellerRoot+"/orders", token, `{"sku":"KEY","quantity":1}`, "optional-channel")
	requireResellerStatus(t, w, http.StatusCreated)
	if len(a.store.data.Orders) != 1 || a.walletAccount(42).BalanceCents != 1550 {
		t.Fatal("optional reseller purchase did not charge exactly once")
	}
}

func TestExplicitChannelMembershipPolicy(t *testing.T) {
	for _, status := range []string{"creator", "administrator", "member", "restricted", "left", "kicked", "unknown", "offline"} {
		t.Run(status, func(t *testing.T) {
			a := &App{cfg: Config{Channel: "@updates", RequireChannel: true, OwnerID: 1}}
			a.tg = &TG{client: &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
				if status == "offline" {
					return nil, errors.New("offline")
				}
				body, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"status": status, "is_member": status == "restricted"}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
			})}}
			joined, err := a.channelMember(context.Background(), 42)
			want := status == "creator" || status == "administrator" || status == "member" || status == "restricted"
			if joined != want || (err != nil) != (status == "offline") {
				t.Fatalf("status=%s joined=%t err=%v", status, joined, err)
			}
		})
	}
}

func TestChannelConfiguration(t *testing.T) {
	t.Setenv("OWNER_TELEGRAM_ID", "1")
	t.Setenv("BOT_TOKEN", "test-token")
	for _, key := range []string{"BEP20_USDT_ADDRESS", "POLYGON_USDT_ADDRESS", "BEP20_USDT_CONTRACT", "POLYGON_USDT_CONTRACT"} {
		t.Setenv(key, "0x"+strings.Repeat("1", 40))
	}
	t.Setenv("MINI_APP_PUBLIC_URL", "")
	t.Setenv("SALES_CHANNEL_URL", "")
	t.Setenv("SALES_CHANNEL", "  @updates  ")
	for _, raw := range []string{"", "false", "true", "invalid"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("REQUIRE_CHANNEL_MEMBERSHIP", raw)
			cfg, err := config()
			if (err != nil) != (raw == "invalid") {
				t.Fatalf("value=%q config error=%v", raw, err)
			}
			if err == nil && (cfg.RequireChannel != (raw == "true") || cfg.Channel != "@updates") {
				t.Fatalf("wrong policy or channel normalization for %q", raw)
			}
		})
	}
	t.Setenv("REQUIRE_CHANNEL_MEMBERSHIP", "true")
	t.Setenv("SALES_CHANNEL", "")
	if _, err := config(); err == nil {
		t.Fatal("enabled gate accepted missing channel")
	}
}

func TestChannelJoinLinks(t *testing.T) {
	for _, test := range []struct{ channel, explicit, want string }{
		{"@updates", "", "https://t.me/updates"},
		{"-100123", "", ""},
		{"", "", ""},
		{"@https://t.me/updates", "", ""},
		{"-100123", "https://t.me/+exampleInvite", "https://t.me/+exampleInvite"},
	} {
		a := &App{cfg: Config{Channel: test.channel, ChannelJoinURL: test.explicit}}
		if got := a.channelURL(); got != test.want {
			t.Errorf("channel=%q got=%q want=%q", test.channel, got, test.want)
		}
	}
	for _, raw := range []string{"http://t.me/updates", "https://example.com/updates", "https://t.me/", "https://t.me/updates?secret=value", "https://user:pass@t.me/updates"} {
		if validChannelJoinURL(raw) {
			t.Errorf("accepted invalid join URL %q", raw)
		}
	}
}

func TestTelegramTransportErrorRedactsToken(t *testing.T) {
	tg := &TG{token: "sensitive-token", client: &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}}
	err := tg.call(context.Background(), "getUpdates", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), tg.token) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error exposed token or lost timeout cause: %v", err)
	}
}
