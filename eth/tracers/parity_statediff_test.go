package tracers

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func sdBig(n int64) *hexutil.Big       { return (*hexutil.Big)(big.NewInt(n)) }
func sdU64(n uint64) *uint64           { return &n }
func sdBytes(b ...byte) *hexutil.Bytes { x := hexutil.Bytes(b); return &x }

// accountJSON marshals one account diff and returns its JSON string for substring
// assertions.
func accountJSON(t *testing.T, diff parityStateDiff, addr common.Address) string {
	t.Helper()
	acc, ok := diff[addr]
	if !ok {
		t.Fatalf("address %s missing from stateDiff", addr.Hex())
	}
	b, err := json.Marshal(acc)
	if err != nil {
		t.Fatalf("marshal account diff: %v", err)
	}
	return string(b)
}

func TestBuildParityStateDiff(t *testing.T) {
	t.Parallel()

	modified := common.HexToAddress("0x1111111111111111111111111111111111111111")
	created := common.HexToAddress("0x2222222222222222222222222222222222222222")
	deleted := common.HexToAddress("0x3333333333333333333333333333333333333333")

	slotChanged := common.HexToHash("0x01")
	slotAdded := common.HexToHash("0x02")
	slotRemoved := common.HexToHash("0x03")

	pre := map[common.Address]*prestateAccount{
		modified: {
			Balance: sdBig(100),
			Nonce:   sdU64(1),
			Storage: map[common.Hash]common.Hash{
				slotChanged: common.HexToHash("0xaa"),
				slotRemoved: common.HexToHash("0xbb"),
			},
		},
		// created: empty pre-state
		created: {Balance: sdBig(0)},
		// deleted: present in pre, absent in post
		deleted: {Balance: sdBig(7), Nonce: sdU64(4)},
	}
	post := map[common.Address]*prestateAccount{
		modified: {
			Balance: sdBig(50), // changed
			// nonce absent -> unchanged
			Storage: map[common.Hash]common.Hash{
				slotChanged: common.HexToHash("0xcc"),
				slotAdded:   common.HexToHash("0xdd"),
			},
		},
		created: {Balance: sdBig(999), Nonce: sdU64(1), Code: sdBytes(0x60, 0x00)},
	}

	diff := buildParityStateDiff(pre, post)

	if len(diff) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(diff))
	}

	// modified: balance "*", nonce "=", storage slot changed/added/removed
	mj := accountJSON(t, diff, modified)
	for _, want := range []string{
		`"balance":{"*":`, `"nonce":"="`,
		// On an existing account all storage changes are "*" (new slot reads as
		// from 0x0; cleared slot reads as to 0x0).
		strings.ToLower(slotChanged.Hex()) + `":{"*":`,
		strings.ToLower(slotAdded.Hex()) + `":{"*":`,
		strings.ToLower(slotRemoved.Hex()) + `":{"*":`,
	} {
		if !strings.Contains(mj, want) {
			t.Errorf("modified diff missing %q in %s", want, mj)
		}
	}

	// created: all fields "+"
	cj := accountJSON(t, diff, created)
	for _, want := range []string{`"balance":{"+":`, `"nonce":{"+":`, `"code":{"+":`} {
		if !strings.Contains(cj, want) {
			t.Errorf("created diff missing %q in %s", want, cj)
		}
	}

	// deleted: all fields "-"
	dj := accountJSON(t, diff, deleted)
	for _, want := range []string{`"balance":{"-":`, `"nonce":{"-":`, `"code":{"-":`} {
		if !strings.Contains(dj, want) {
			t.Errorf("deleted diff missing %q in %s", want, dj)
		}
	}
}

// TestReplayTransactionParity_StateDiff exercises the stateDiff trace type end to
// end: a value transfer must show the sender's balance and nonce changing.
func TestReplayTransactionParity_StateDiff(t *testing.T) {
	t.Parallel()
	api, target, from := replayTxNewTestAPI(t)

	res, err := api.ReplayTransaction(t.Context(), target, []string{"stateDiff"})
	if err != nil {
		t.Fatalf("trace_replayTransaction stateDiff error: %v", err)
	}
	if res.StateDiff == nil {
		t.Fatalf("expected non-nil stateDiff")
	}
	if res.Trace != nil {
		t.Errorf("trace must be nil when only stateDiff requested, got %v", res.Trace)
	}
	// trace_replayTransaction carries the replayed tx hash (erigon parity).
	if res.TransactionHash == nil || *res.TransactionHash != target {
		t.Errorf("expected transactionHash %x, got %v", target, res.TransactionHash)
	}

	b, err := json.Marshal(res.StateDiff)
	if err != nil {
		t.Fatalf("marshal stateDiff: %v", err)
	}
	var sd map[string]map[string]json.RawMessage
	if err := json.Unmarshal(b, &sd); err != nil {
		t.Fatalf("unmarshal stateDiff: %v", err)
	}

	acc, ok := sd[strings.ToLower(from.Hex())]
	if !ok {
		t.Fatalf("sender %s not present in stateDiff: %s", from.Hex(), string(b))
	}
	if string(acc["balance"]) == `"="` {
		t.Errorf("sender balance should have changed, got %s", string(b))
	}
	if string(acc["nonce"]) == `"="` {
		t.Errorf("sender nonce should have changed, got %s", string(b))
	}
}
