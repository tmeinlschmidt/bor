package tracers

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// replayTxNewTestAPI builds a small chain with a single value-transfer tx in the
// head block and returns the trace API plus that tx's hash.
func replayTxNewTestAPI(t *testing.T) (*TraceAPI, common.Hash, common.Address) {
	t.Helper()
	accounts := newAccounts(2)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	genBlocks := 3
	signer := types.HomesteadSigner{}
	var target common.Hash
	backend := newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    uint64(i),
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
		}), signer, accounts[0].key)
		b.AddTx(tx)
		if i == genBlocks-1 {
			target = tx.Hash()
		}
	})
	t.Cleanup(backend.teardown)
	return &TraceAPI{API: NewAPI(backend)}, target, accounts[0].addr
}

func TestReplayTransactionParity(t *testing.T) {
	t.Parallel()
	api, target, from := replayTxNewTestAPI(t)

	res, err := api.ReplayTransaction(t.Context(), target, []string{"trace"})
	if err != nil {
		t.Fatalf("trace_replayTransaction error: %v", err)
	}
	if res.Output == nil {
		t.Fatalf("expected non-nil output")
	}
	if res.StateDiff != nil || res.VMTrace != nil {
		t.Errorf("stateDiff/vmTrace must be nil in trace-only phase, got %+v / %+v", res.StateDiff, res.VMTrace)
	}
	if len(res.Trace) == 0 {
		t.Fatalf("expected at least one trace entry")
	}
	root := res.Trace[0]
	if root.Type != "call" {
		t.Errorf("expected root type 'call', got %q", root.Type)
	}
	// Replay semantics: no per-tx/block identifiers on entries.
	if root.TransactionHash != nil || root.BlockHash != nil || root.TransactionPosition != nil || root.BlockNumber != nil {
		t.Errorf("replay traces must omit tx/block metadata, got %+v", root)
	}
	if root.Action == nil || root.Action.From == nil || *root.Action.From != from {
		t.Errorf("expected from %x, got %+v", from, root.Action)
	}
}

func TestReplayTransactionParity_NoTraceType(t *testing.T) {
	t.Parallel()
	api, target, _ := replayTxNewTestAPI(t)

	res, err := api.ReplayTransaction(t.Context(), target, []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Output == nil {
		t.Errorf("expected output even with no trace types")
	}
	if len(res.Trace) != 0 {
		t.Errorf("expected no trace entries when 'trace' not requested, got %d", len(res.Trace))
	}
}

func TestReplayTransactionParity_Errors(t *testing.T) {
	t.Parallel()
	api, target, _ := replayTxNewTestAPI(t)

	if _, err := api.ReplayTransaction(t.Context(), target, []string{"bogus"}); err == nil {
		t.Errorf("expected error for unknown trace type")
	}
	if _, err := api.ReplayTransaction(t.Context(), crypto.Keccak256Hash([]byte("missing")), []string{"trace"}); err == nil {
		t.Errorf("expected error for unknown transaction")
	}
}
