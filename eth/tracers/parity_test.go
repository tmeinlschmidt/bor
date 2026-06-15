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

func TestParseTraceTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   []string
		want    traceTypeSet
		wantErr bool
	}{
		{name: "empty", input: nil, want: traceTypeSet{}},
		{name: "trace only", input: []string{"trace"}, want: traceTypeSet{trace: true}},
		{name: "all", input: []string{"trace", "stateDiff", "vmTrace"}, want: traceTypeSet{trace: true, stateDiff: true, vmTrace: true}},
		{name: "duplicate", input: []string{"trace", "trace"}, want: traceTypeSet{trace: true}},
		{name: "unknown", input: []string{"bogus"}, wantErr: true},
		{name: "unknown mixed", input: []string{"trace", "bogus"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTraceTypes(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestStripTxMeta(t *testing.T) {
	t.Parallel()

	hash := common.HexToHash("0xdead")
	num := uint64(7)
	pos := uint64(1)
	traces := []*ParityTrace{{
		BlockHash:           &hash,
		BlockNumber:         &num,
		TransactionHash:     &hash,
		TransactionPosition: &pos,
		Type:                "call",
	}}
	stripTxMeta(traces)
	tr := traces[0]
	if tr.BlockHash != nil || tr.BlockNumber != nil || tr.TransactionHash != nil || tr.TransactionPosition != nil {
		t.Fatalf("expected all tx/block meta cleared, got %+v", tr)
	}
}

// TestTraceTransactionParity exercises trace_transaction end to end against a
// synthetic chain and asserts the flat Parity output carries block/tx metadata.
func TestTraceTransactionParity(t *testing.T) {
	t.Parallel()

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
	defer backend.teardown()

	api := NewAPI(backend)
	traceAPI := &TraceAPI{API: api}

	traces, err := traceAPI.Transaction(t.Context(), target)
	if err != nil {
		t.Fatalf("trace_transaction error: %v", err)
	}
	if len(traces) == 0 {
		t.Fatalf("expected at least one trace, got 0")
	}
	root := traces[0]
	if root.Type != "call" {
		t.Errorf("expected root type 'call', got %q", root.Type)
	}
	if len(root.TraceAddress) != 0 {
		t.Errorf("root traceAddress should be empty, got %v", root.TraceAddress)
	}
	// trace_transaction must carry block/tx identifiers.
	if root.TransactionHash == nil || *root.TransactionHash != target {
		t.Errorf("expected transactionHash %x, got %v", target, root.TransactionHash)
	}
	if root.BlockHash == nil || root.BlockNumber == nil || root.TransactionPosition == nil {
		t.Errorf("expected block/tx metadata to be populated, got %+v", root)
	}
	if root.Action == nil || root.Action.From == nil || *root.Action.From != accounts[0].addr {
		t.Errorf("expected from %x, got %+v", accounts[0].addr, root.Action)
	}
}

func TestTraceTransactionParity_NotFound(t *testing.T) {
	t.Parallel()

	accounts := newAccounts(1)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{accounts[0].addr: {Balance: big.NewInt(params.Ether)}},
	}
	backend := newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {})
	defer backend.teardown()

	traceAPI := &TraceAPI{API: NewAPI(backend)}
	_, err := traceAPI.Transaction(t.Context(), crypto.Keccak256Hash([]byte("missing")))
	if err == nil {
		t.Fatalf("expected error for unknown transaction, got nil")
	}
}
