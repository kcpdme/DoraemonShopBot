package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

var defaultBSCRPCs = []string{"https://bsc-dataseed.bnbchain.org", "https://bsc-rpc.publicnode.com", "https://1rpc.io/bnb", "https://bsc.drpc.org"}
var defaultPolygonRPCs = []string{"https://polygon-bor-rpc.publicnode.com", "https://1rpc.io/matic", "https://polygon.drpc.org"}

// Permanent failures require another transaction or support, never a retry of
// the same hash. Ordinary errors are provider/configuration failures.
type permanentPaymentError struct{ reason string }

func (e *permanentPaymentError) Error() string { return e.reason }
func isPermanentPaymentError(err error) bool {
	var target *permanentPaymentError
	return errors.As(err, &target)
}
func permanentPaymentFailure(reason string) (bool, string, error) {
	return false, reason, &permanentPaymentError{reason: reason}
}

func (a *App) checkPayment(ctx context.Context, o Order) (bool, string, error) {
	if !validHash(o.TxHash) {
		return permanentPaymentFailure("This transaction ID is invalid. Submit the full transaction ID from your wallet.")
	}
	rpcs, wallet, token, decimals, chainID := rpcList(a.cfg.BSCRPCURL, defaultBSCRPCs), a.cfg.BEP20Address, a.cfg.BEP20USDT, 18, uint64(56)
	switch o.Network {
	case "bep20":
	case "polygon":
		rpcs, wallet, token, decimals, chainID = rpcList(a.cfg.PolygonRPCURL, defaultPolygonRPCs), a.cfg.PolygonAddress, a.cfg.PolygonUSDT, 6, 137
	default:
		return false, "", errors.New("unsupported order network")
	}
	if !validHexBytes(wallet, 20) || !validHexBytes(token, 20) || o.CreatedAt.IsZero() {
		return false, "", errors.New("invalid payment configuration or order creation time")
	}
	if _, err := usdtUnits(o.Amount, decimals); err != nil {
		return false, "", err
	}
	// Bound the whole verification as well as each provider, including fallbacks.
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var last error
	missing := false
	for _, endpoint := range rpcs {
		if ctx.Err() != nil {
			break
		}
		endpointCtx, endpointCancel := context.WithTimeout(ctx, 7*time.Second)
		ok, reason, err := a.checkPaymentAt(endpointCtx, endpoint, o, wallet, token, decimals, chainID)
		endpointCancel()
		if isPermanentPaymentError(err) || ok {
			return ok, reason, err
		}
		if err == nil {
			if reason == "Transaction not visible on this network yet." {
				missing = true
				continue
			}
			return false, reason, nil
		}
		last = err
	}
	if missing {
		return false, "Transaction not visible on this network yet.", nil
	}
	if last == nil {
		last = errors.New("no available RPC endpoint")
	}
	return false, "", last
}

func (a *App) checkPaymentAt(ctx context.Context, endpoint string, o Order, wallet, token string, decimals int, chainID uint64) (bool, string, error) {
	rawChain, err := a.rpcString(ctx, endpoint, "eth_chainId", []any{})
	if err != nil {
		return false, "", err
	}
	actualChain, err := parseHexUint(rawChain)
	if err != nil || actualChain != chainID {
		return false, "", errors.New("RPC returned wrong chain ID")
	}
	receipt, err := a.rpc(ctx, endpoint, "eth_getTransactionReceipt", []any{o.TxHash})
	if err != nil {
		return false, "", err
	}
	if receipt == nil {
		return false, "Transaction not visible on this network yet.", nil
	}
	hash, _ := receipt["transactionHash"].(string)
	if !validHash(hash) || !strings.EqualFold(hash, o.TxHash) {
		return false, "", errors.New("RPC returned mismatched transaction receipt")
	}
	rawStatus, _ := receipt["status"].(string)
	status, err := parseHexUint(rawStatus)
	if err != nil || status > 1 {
		return false, "", errors.New("invalid transaction receipt status")
	}
	if status == 0 {
		return permanentPaymentFailure("This transaction failed on-chain. Submit a successful payment transaction or contact support.")
	}
	block, _ := receipt["blockNumber"].(string)
	blockNumber, err := parseHexUint(block)
	if err != nil {
		return false, "", errors.New("invalid receipt block number")
	}
	blockData, err := a.rpc(ctx, endpoint, "eth_getBlockByNumber", []any{block, false})
	if err != nil {
		return false, "", err
	}
	if blockData == nil {
		return false, "", errors.New("transaction block unavailable")
	}
	if receiptBlockHash, _ := receipt["blockHash"].(string); receiptBlockHash != "" {
		canonicalHash, _ := blockData["hash"].(string)
		if !validHash(receiptBlockHash) || !strings.EqualFold(canonicalHash, receiptBlockHash) {
			return false, "", errors.New("transaction block changed; waiting for canonical receipt")
		}
	}
	rawTimestamp, _ := blockData["timestamp"].(string)
	timestamp, err := parseHexUint(rawTimestamp)
	if err != nil || timestamp == 0 || timestamp > math.MaxInt64 {
		return false, "", errors.New("invalid transaction block timestamp")
	}
	blockTime := time.Unix(int64(timestamp), 0).UTC()
	if blockTime.After(time.Now().Add(5 * time.Minute)) {
		return false, "", errors.New("RPC returned future block timestamp")
	}
	if blockTime.Before(o.CreatedAt.Add(-5 * time.Minute)) {
		return permanentPaymentFailure("This transaction was made before this order and cannot pay for it. Submit the transaction for this order, or contact support if you already paid.")
	}
	logs, ok := receipt["logs"].([]any)
	if !ok {
		return false, "", errors.New("transaction receipt logs unavailable")
	}
	total := new(big.Int)
	matched := false
	for _, raw := range logs {
		lg, ok := raw.(map[string]any)
		if !ok {
			return false, "", errors.New("invalid receipt log")
		}
		contract, _ := lg["address"].(string)
		if !strings.EqualFold(contract, token) {
			continue
		}
		if removed, _ := lg["removed"].(bool); removed {
			return false, "", errors.New("transaction log removed; waiting for canonical receipt")
		}
		topics, _ := lg["topics"].([]any)
		if len(topics) != 3 {
			continue
		}
		topic0, _ := topics[0].(string)
		recipientTopic, _ := topics[2].(string)
		if !strings.EqualFold(topic0, transferTopic) || !strings.EqualFold(topicAddress(recipientTopic), wallet) {
			continue
		}
		rawAmount, _ := lg["data"].(string)
		if !validHexBytes(rawAmount, 32) {
			return false, "", errors.New("invalid token transfer amount")
		}
		amount, _ := new(big.Int).SetString(rawAmount[2:], 16)
		total.Add(total, amount)
		matched = true
	}
	if !matched {
		return permanentPaymentFailure("This transaction has no USDT payment to the wallet shown for this order. Check the network, token and receiving wallet, or contact support.")
	}
	expected, _ := usdtUnits(o.Amount, decimals)
	if total.Cmp(expected) != 0 {
		return permanentPaymentFailure("The USDT amount does not match this order. Contact support if you sent a different amount; do not pay again without checking.")
	}
	latestHex, err := a.rpcString(ctx, endpoint, "eth_blockNumber", []any{})
	if err != nil {
		return false, "", err
	}
	latest, err := parseHexUint(latestHex)
	if err != nil {
		return false, "", err
	}
	if latest < blockNumber {
		return false, "", errors.New("RPC is behind the transaction block")
	}
	if latest-blockNumber < 2 {
		return false, fmt.Sprintf("Payment found — %d/3 confirmations. Your items will arrive automatically after confirmation.", latest-blockNumber+1), nil
	}
	return true, "", nil
}

func rpcList(primary string, fallbacks []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, u := range append([]string{primary}, fallbacks...) {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}
func validHexBytes(s string, size int) bool {
	if len(s) != 2+size*2 || !strings.EqualFold(s[:2], "0x") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}
func validHash(s string) bool { return validHexBytes(s, 32) }
func normalizeHash(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(strings.ToLower(s), "/tx/"); i >= 0 {
		s = s[i+4:]
	}
	if i := strings.IndexAny(s, "?#&"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}
func parseHexUint(s string) (uint64, error) {
	if len(s) < 3 || !strings.EqualFold(s[:2], "0x") {
		return 0, errors.New("invalid hexadecimal quantity")
	}
	return strconv.ParseUint(s[2:], 16, 64)
}
func topicAddress(s string) string {
	if !validHexBytes(s, 32) || s[2:26] != strings.Repeat("0", 24) {
		return ""
	}
	return "0x" + strings.ToLower(s[26:])
}
func usdtUnits(price float64, decimals int) (*big.Int, error) {
	if math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 || decimals < 2 || decimals > 18 {
		return nil, errors.New("invalid USDT amount")
	}
	// Prices are stored as two-decimal amounts; convert their displayed decimal
	// representation directly to integer units without multiplying floats.
	cents, ok := new(big.Int).SetString(strings.ReplaceAll(strconv.FormatFloat(price, 'f', 2, 64), ".", ""), 10)
	if !ok || cents.Sign() <= 0 {
		return nil, errors.New("invalid USDT amount")
	}
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-2)), nil)
	return new(big.Int).Mul(cents, factor), nil
}
func exactHexUSDTAmount(price float64, raw string, decimals int) bool {
	if len(raw) < 3 || !strings.EqualFold(raw[:2], "0x") {
		return false
	}
	actual, ok := new(big.Int).SetString(raw[2:], 16)
	expected, err := usdtUnits(price, decimals)
	return ok && err == nil && actual.Cmp(expected) == 0
}
func exactUSDTAmount(price float64, raw, decimals string) bool {
	d, err := strconv.Atoi(decimals)
	if err != nil {
		return false
	}
	expected, err := usdtUnits(price, d)
	actual, ok := new(big.Int).SetString(raw, 10)
	return err == nil && ok && actual.Cmp(expected) == 0
}

func (a *App) rpcRaw(ctx context.Context, endpoint, method string, params []any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "DoraemonShop-Deployment/1.0")
	r, err := a.tg.client.Do(req)
	if err != nil {
		return nil, errors.New("RPC connection unavailable")
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("RPC %s HTTP %d", method, r.StatusCode)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&out); err != nil {
		return nil, errors.New("invalid RPC response")
	}
	if out.Error != nil {
		return nil, fmt.Errorf("RPC %s error %d", method, out.Error.Code)
	}
	if len(out.Result) == 0 {
		return nil, errors.New("RPC result missing")
	}
	return out.Result, nil
}
func (a *App) rpc(ctx context.Context, endpoint, method string, params []any) (map[string]any, error) {
	raw, err := a.rpcRaw(ctx, endpoint, method, params)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(raw, &result)
	return result, err
}
func (a *App) rpcString(ctx context.Context, endpoint, method string, params []any) (string, error) {
	raw, err := a.rpcRaw(ctx, endpoint, method, params)
	if err != nil {
		return "", err
	}
	var result string
	if err = json.Unmarshal(raw, &result); err != nil || result == "" {
		return "", errors.New("invalid RPC string result")
	}
	return result, nil
}
