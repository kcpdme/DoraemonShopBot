package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type paymentTransport func(*http.Request) (*http.Response, error)

func (f paymentTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func paymentFixture(t *testing.T, network string, mutate func(map[string]any)) (*App, Order) {
	t.Helper()
	wallet := "0x" + strings.Repeat("a", 40)
	token := "0x" + strings.Repeat("b", 40)
	o := Order{Network: network, TxHash: "0x" + strings.Repeat("c", 64), Amount: 12.34, CreatedAt: time.Now().Add(-time.Minute)}
	decimals, chain := 18, "0x38"
	if network == "polygon" {
		decimals, chain = 6, "0x89"
	}
	amount, err := usdtUnits(o.Amount, decimals)
	if err != nil {
		t.Fatal(err)
	}
	replies := map[string]any{
		"eth_chainId": chain,
		"eth_getTransactionReceipt": map[string]any{
			"transactionHash": o.TxHash, "status": "0x1", "blockNumber": "0x64",
			"logs": []any{map[string]any{"address": token, "topics": []any{transferTopic, "0x" + strings.Repeat("0", 64), "0x" + strings.Repeat("0", 24) + wallet[2:]}, "data": fmt.Sprintf("0x%064x", amount)}},
		},
		"eth_getBlockByNumber": map[string]any{"timestamp": fmt.Sprintf("0x%x", time.Now().Unix())},
		"eth_blockNumber":      "0x66",
	}
	if mutate != nil {
		mutate(replies)
	}
	client := &http.Client{Transport: paymentTransport(func(r *http.Request) (*http.Response, error) {
		var req struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		value, ok := replies[req.Method]
		if !ok {
			t.Errorf("unexpected RPC method %s", req.Method)
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": value})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
	})}
	a := &App{cfg: Config{BEP20Address: wallet, PolygonAddress: wallet, BEP20USDT: token, PolygonUSDT: token}, tg: &TG{client: client}}
	return a, o
}

func TestPaymentVerification(t *testing.T) {
	for _, network := range []string{"bep20", "polygon"} {
		t.Run(network, func(t *testing.T) {
			a, o := paymentFixture(t, network, nil)
			ok, reason, err := a.checkPayment(context.Background(), o)
			if !ok || err != nil {
				t.Fatalf("valid payment: ok=%v reason=%q err=%v", ok, reason, err)
			}
		})
	}
	tests := []struct {
		name               string
		mutate             func(map[string]any)
		permanent, pending bool
	}{
		{"predates order", func(r map[string]any) {
			r["eth_getBlockByNumber"].(map[string]any)["timestamp"] = fmt.Sprintf("0x%x", time.Now().Add(-time.Hour).Unix())
		}, true, false},
		{"failed transfer", func(r map[string]any) { r["eth_getTransactionReceipt"].(map[string]any)["status"] = "0x0" }, true, false},
		{"wrong wallet", func(r map[string]any) {
			r["eth_getTransactionReceipt"].(map[string]any)["logs"].([]any)[0].(map[string]any)["topics"].([]any)[2] = "0x" + strings.Repeat("0", 64)
		}, true, false},
		{"wrong token", func(r map[string]any) {
			r["eth_getTransactionReceipt"].(map[string]any)["logs"].([]any)[0].(map[string]any)["address"] = "0x" + strings.Repeat("d", 40)
		}, true, false},
		{"wrong amount", func(r map[string]any) {
			r["eth_getTransactionReceipt"].(map[string]any)["logs"].([]any)[0].(map[string]any)["data"] = "0x" + strings.Repeat("0", 64)
		}, true, false},
		{"missing transaction", func(r map[string]any) { r["eth_getTransactionReceipt"] = nil }, false, true},
		{"two confirmations", func(r map[string]any) { r["eth_blockNumber"] = "0x65" }, false, true},
		{"latest behind receipt prevents underflow", func(r map[string]any) { r["eth_blockNumber"] = "0x63" }, false, false},
		{"wrong RPC chain", func(r map[string]any) { r["eth_chainId"] = "0x1" }, false, false},
		{"missing block timestamp fails closed", func(r map[string]any) { r["eth_getBlockByNumber"] = map[string]any{} }, false, false},
		{"invalid block timestamp fails closed", func(r map[string]any) { r["eth_getBlockByNumber"].(map[string]any)["timestamp"] = "0xzz" }, false, false},
		{"receipt hash mismatch", func(r map[string]any) {
			r["eth_getTransactionReceipt"].(map[string]any)["transactionHash"] = "0x" + strings.Repeat("f", 64)
		}, false, false},
		{"malformed amount", func(r map[string]any) {
			r["eth_getTransactionReceipt"].(map[string]any)["logs"].([]any)[0].(map[string]any)["data"] = "0xzz"
		}, false, false},
		{"missing logs fail closed", func(r map[string]any) { delete(r["eth_getTransactionReceipt"].(map[string]any), "logs") }, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, o := paymentFixture(t, "bep20", test.mutate)
			ok, reason, err := a.checkPayment(context.Background(), o)
			if ok {
				t.Fatal("invalid or pending payment accepted")
			}
			if isPermanentPaymentError(err) != test.permanent {
				t.Fatalf("permanent=%v, reason=%q, err=%v", test.permanent, reason, err)
			}
			if (err == nil) != test.pending {
				t.Fatalf("pending=%v, reason=%q, err=%v", test.pending, reason, err)
			}
		})
	}
}

func TestPaymentStrictHashAndAmount(t *testing.T) {
	if validHash("0x" + strings.Repeat("z", 64)) {
		t.Fatal("non-hex hash accepted")
	}
	if validHash(" " + "0x" + strings.Repeat("a", 64)) {
		t.Fatal("non-canonical hash accepted")
	}
	if topicAddress("0x"+strings.Repeat("f", 64)) != "" {
		t.Fatal("non-canonical ABI address accepted")
	}
	for _, decimals := range []int{6, 18} {
		units, err := usdtUnits(12.34, decimals)
		if err != nil || !exactHexUSDTAmount(12.34, fmt.Sprintf("0x%x", units), decimals) {
			t.Fatalf("decimal conversion failed %d", decimals)
		}
	}
}

func TestPaymentRPCFallback(t *testing.T) {
	a, o := paymentFixture(t, "polygon", nil)
	base := a.tg.client.Transport
	a.cfg.PolygonRPCURL = "https://unavailable.example"
	failed := 0
	a.tg.client.Transport = paymentTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "unavailable.example" {
			failed++
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("rate limited"))}, nil
		}
		return base.RoundTrip(r)
	})
	ok, _, err := a.checkPayment(context.Background(), o)
	if !ok || err != nil || failed != 1 {
		t.Fatalf("fallback: ok=%v err=%v failures=%d", ok, err, failed)
	}
}
