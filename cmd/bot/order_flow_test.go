package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type lifecycleRecorder struct {
	mu               sync.Mutex
	rpcCalls         int
	deliveries       []string
	failInstructions bool
}

func lifecycleFixture(t *testing.T, mutate func(map[string]any)) (*App, Order, *lifecycleRecorder) {
	t.Helper()
	a, o := paymentFixture(t, "bep20", mutate)
	s, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	a.store = s
	o.ID = "order-fixture"
	o.SKU = "PRODUCT"
	o.Quantity = 1
	o.BuyerID = 123
	o.Status = "payment_submitted"
	o.ProductName = "Digital product"
	o.DeliveryInstruction = "Use the supplied private key"
	o.StockIDs = []string{"stock-1"}
	s.data.Products[o.SKU] = Product{SKU: o.SKU, Name: o.ProductName, PriceUSDT: o.Amount, DeliveryInstruction: o.DeliveryInstruction, Active: true}
	s.data.Stock["stock-1"] = StockItem{ID: "stock-1", SKU: o.SKU, OrderID: o.ID, Payload: "private-key-one"}
	s.data.Orders[o.ID] = o
	if err = s.save(); err != nil {
		t.Fatal(err)
	}
	recorder := &lifecycleRecorder{}
	base := a.tg.client.Transport
	a.tg.client.Transport = paymentTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.telegram.org" {
			recorder.mu.Lock()
			recorder.rpcCalls++
			recorder.mu.Unlock()
			return base.RoundTrip(r)
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		if recorder.failInstructions && strings.HasPrefix(body.Text, "📖 Delivery instructions") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":false,"description":"temporary Telegram failure"}`)), Header: http.Header{}}, nil
		}
		if strings.HasPrefix(body.Text, "✅ <b>Your order is ready") || strings.HasPrefix(body.Text, "📖 Delivery instructions") || strings.HasPrefix(body.Text, "🔐 Item") {
			recorder.deliveries = append(recorder.deliveries, body.Text)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":42}}`)), Header: http.Header{}}, nil
	})
	return a, o, recorder
}

func storedOrder(a *App, id string) Order {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	return a.store.data.Orders[id]
}

func awaitOrderStatus(t *testing.T, a *App, id, status string) Order {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		o := storedOrder(a, id)
		if o.Status == status {
			return o
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("order %s: wanted status %s, got %s", id, status, storedOrder(a, id).Status)
	return Order{}
}

func TestPermanentProofStopsRepeatedVerification(t *testing.T) {
	a, o, record := lifecycleFixture(t, func(r map[string]any) { r["eth_getBlockByNumber"].(map[string]any)["timestamp"] = "0x1" })
	a.verifyOrder(context.Background(), o.ID)
	got := storedOrder(a, o.ID)
	if got.Status != "proof_invalid" || !strings.Contains(got.PaymentIssue, "before this order") {
		t.Fatalf("permanent rejection not saved: %+v", got)
	}
	record.mu.Lock()
	before := record.rpcCalls
	record.mu.Unlock()
	a.verifyOrder(context.Background(), o.ID)
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.rpcCalls != before {
		t.Fatalf("permanently rejected hash retried: before %d after %d", before, record.rpcCalls)
	}
	if len(record.deliveries) != 0 {
		t.Fatal("invalid proof delivered inventory")
	}
}

func TestRejectedProofCanBeReplacedAndDelivered(t *testing.T) {
	a, o, _ := lifecycleFixture(t, nil)
	validHash := o.TxHash
	o.Status = "proof_invalid"
	o.TxHash = "0x" + strings.Repeat("f", 64)
	o.PaymentIssue = "Old transaction"
	a.store.data.Orders[o.ID] = o
	if err := a.store.save(); err != nil {
		t.Fatal(err)
	}
	a.submitTxHash(context.Background(), o.BuyerID, o.BuyerID, o.ID, "https://bscscan.com/tx/"+validHash)
	got := awaitOrderStatus(t, a, o.ID, "delivered")
	// Wait for the async verification's final notifications before fixture cleanup.
	a.opMu.Lock()
	a.opMu.Unlock()
	if got.TxHash != validHash || got.PaymentIssue != "" {
		t.Fatalf("replacement did not complete: %+v", got)
	}
}

func TestDeliveryFailureRetriesSavedCursorWithoutRPC(t *testing.T) {
	a, o, record := lifecycleFixture(t, nil)
	record.failInstructions = true
	a.verifyOrder(context.Background(), o.ID)
	got := storedOrder(a, o.ID)
	if got.Status != "delivery_pending" || got.DeliveryPartsSent != 1 {
		t.Fatalf("delivery failure lost cursor: %+v", got)
	}
	disk, err := openStore(a.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.data.Orders[o.ID].DeliveryPartsSent != 1 {
		t.Fatal("delivery cursor not persisted")
	}
	if disk.data.Stock[o.StockIDs[0]].Sold {
		t.Fatal("stock marked sold before delivery")
	}
	record.mu.Lock()
	before := record.rpcCalls
	record.failInstructions = false
	record.mu.Unlock()
	a.verifyOrder(context.Background(), o.ID)
	if got = storedOrder(a, o.ID); got.Status != "delivered" {
		t.Fatalf("retry did not deliver: %+v", got)
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.rpcCalls != before {
		t.Fatal("confirmed payment was verified again during delivery retry")
	}
	if len(record.deliveries) != 3 {
		t.Fatalf("delivery parts duplicated or missing: %v", record.deliveries)
	}
	if !strings.Contains(record.deliveries[0], "Your order is ready") || !strings.Contains(record.deliveries[1], o.DeliveryInstruction) || !strings.Contains(record.deliveries[2], "private-key-one") {
		t.Fatalf("unexpected delivery order: %v", record.deliveries)
	}
}

func TestSimultaneousChecksDeliverOnce(t *testing.T) {
	a, o, record := lifecycleFixture(t, nil)
	base := a.tg.client.Transport
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	a.tg.client.Transport = paymentTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.telegram.org" {
			once.Do(func() { close(entered); <-release })
		}
		return base.RoundTrip(r)
	})
	go func() { defer close(finished); a.verifyOrder(context.Background(), o.ID) }()
	<-entered
	var contenders sync.WaitGroup
	for i := 0; i < 12; i++ {
		contenders.Add(1)
		go func() { defer contenders.Done(); a.verifyOrder(context.Background(), o.ID) }()
	}
	contenders.Wait()
	close(release)
	<-finished
	if got := storedOrder(a, o.ID); got.Status != "delivered" {
		t.Fatalf("not delivered: %+v", got)
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if len(record.deliveries) != 3 {
		t.Fatalf("duplicate delivery: %d successful parts", len(record.deliveries))
	}
	if record.rpcCalls != 4 {
		t.Fatalf("duplicate chain checks: %d RPC calls", record.rpcCalls)
	}
}

func TestQuantityReservationDoubleTapAndRollback(t *testing.T) {
	a, o, _ := lifecycleFixture(t, nil)
	a.store.data.Orders = map[string]Order{}
	a.store.data.Stock = map[string]StockItem{
		"a": {ID: "a", SKU: o.SKU, Payload: "one"},
		"b": {ID: "b", SKU: o.SKU, Payload: "two"},
		"c": {ID: "c", SKU: o.SKU, Payload: "three"},
	}
	if err := a.store.save(); err != nil {
		t.Fatal(err)
	}
	buyer := &User{ID: o.BuyerID, FirstName: "Buyer"}
	first, err := a.createOrderQuantity(buyer, o.SKU, "bep20", 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.createOrderQuantity(buyer, o.SKU, "bep20", 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Amount != 24.68 || len(first.StockIDs) != 2 || len(a.store.data.Orders) != 1 {
		t.Fatalf("double tap created another reservation: first=%+v second=%+v", first, second)
	}
	if _, err = a.createOrderQuantity(&User{ID: 456}, o.SKU, "polygon", 2); err == nil {
		t.Fatal("oversold reserved stock")
	}
	// A file used as a parent directory forces a deterministic persistence failure,
	// including under root, without relying on permission bits.
	originalPath := a.store.path
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err = os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	a.store.path = filepath.Join(blocked, "store.json")
	if _, err = a.createOrderQuantity(&User{ID: 456}, o.SKU, "polygon", 1); err == nil {
		t.Fatal("expected persistence error")
	}
	a.store.path = originalPath
	if len(a.store.data.Orders) != 1 || a.store.data.Stock["c"].OrderID != "" {
		t.Fatal("failed reservation remained in memory after persistence rollback")
	}
	if _, err = a.createOrderQuantity(&User{ID: 456}, o.SKU, "polygon", 1); err != nil {
		t.Fatalf("rolled-back stock could not be reserved: %v", err)
	}
}

func TestReplacingProofDoesNotBlockOnRPCOrAcceptStaleResult(t *testing.T) {
	a, o, record := lifecycleFixture(t, nil)
	base := a.tg.client.Transport
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	a.tg.client.Transport = paymentTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.telegram.org" {
			once.Do(func() { close(entered); <-release })
		}
		return base.RoundTrip(r)
	})
	go func() { defer close(finished); a.verifyOrder(context.Background(), o.ID) }()
	<-entered
	replacement := "0x" + strings.Repeat("a", 64)
	submitted := make(chan struct{})
	go func() {
		defer close(submitted)
		a.submitTxHash(context.Background(), o.BuyerID, o.BuyerID, o.ID, replacement)
	}()
	select {
	case <-submitted:
	case <-time.After(time.Second):
		close(release)
		<-finished
		<-submitted
		t.Fatal("proof replacement blocked on RPC")
	}
	close(release)
	<-finished
	got := storedOrder(a, o.ID)
	if got.TxHash != replacement || got.Status == "delivered" {
		t.Fatalf("stale verification accepted: %+v", got)
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if len(record.deliveries) != 0 {
		t.Fatal("stale proof delivered items")
	}
}
