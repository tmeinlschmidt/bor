// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// replayBlockNewTestAPI builds a hermetic chain whose every block contains a
// single value-transfer transaction, and returns the trace API plus the chosen
// number of generated blocks. The native callTracer is registered for tests via
// register_native_test.go.
func replayBlockNewTestAPI(t *testing.T) (*TraceAPI, int) {
	t.Helper()

	accounts := newAccounts(2)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
		},
	}

	genBlocks := 5
	signer := types.HomesteadSigner{}
	backend := newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    uint64(i),
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
			Data:     nil,
		}), signer, accounts[0].key)
		b.AddTx(tx)
	})
	t.Cleanup(backend.chain.Stop)

	return &TraceAPI{API: NewAPI(backend)}, genBlocks
}

// assertReplayBlockTraceResult validates a single ReplayResult that was produced
// with the "trace" output requested.
func assertReplayBlockTraceResult(t *testing.T, res *ReplayResult) {
	t.Helper()

	if res.Output == nil {
		t.Fatal("expected non-nil Output")
	}
	if len(res.Trace) < 1 {
		t.Fatalf("expected at least 1 trace entry, got %d", len(res.Trace))
	}
	if res.StateDiff != nil {
		t.Errorf("expected nil StateDiff, got %v", res.StateDiff)
	}
	if res.VMTrace != nil {
		t.Errorf("expected nil VMTrace, got %v", res.VMTrace)
	}

	root := res.Trace[0]
	if root.Type != "call" {
		t.Errorf("expected root trace type 'call', got %q", root.Type)
	}
	// trace_replay* entries must not carry block/tx identifier metadata.
	if root.TransactionHash != nil {
		t.Errorf("expected nil TransactionHash on replay trace, got %v", root.TransactionHash)
	}
	if root.TransactionPosition != nil {
		t.Errorf("expected nil TransactionPosition, got %v", root.TransactionPosition)
	}
	if root.BlockHash != nil {
		t.Errorf("expected nil BlockHash, got %v", root.BlockHash)
	}
	if root.BlockNumber != nil {
		t.Errorf("expected nil BlockNumber, got %v", root.BlockNumber)
	}
}

func TestReplayBlockTransactionsByNumber(t *testing.T) {
	t.Parallel()

	api, genBlocks := replayBlockNewTestAPI(t)

	results, err := api.ReplayBlockTransactions(
		t.Context(),
		rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(genBlocks)),
		[]string{"trace"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The generator adds exactly one transaction per block.
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	for _, res := range results {
		assertReplayBlockTraceResult(t, res)
	}
}

func TestReplayBlockTransactionsByHash(t *testing.T) {
	t.Parallel()

	api, genBlocks := replayBlockNewTestAPI(t)

	block, err := api.blockByNumber(t.Context(), rpc.BlockNumber(genBlocks))
	if err != nil {
		t.Fatalf("failed to resolve block: %v", err)
	}

	results, err := api.ReplayBlockTransactions(
		t.Context(),
		rpc.BlockNumberOrHashWithHash(block.Hash(), false),
		[]string{"trace"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	for _, res := range results {
		assertReplayBlockTraceResult(t, res)
	}
}

func TestReplayBlockTransactionsNoTraceTypes(t *testing.T) {
	t.Parallel()

	api, genBlocks := replayBlockNewTestAPI(t)

	results, err := api.ReplayBlockTransactions(
		t.Context(),
		rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(genBlocks)),
		[]string{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	for _, res := range results {
		if res.Output == nil {
			t.Error("expected non-nil Output even without trace types")
		}
		if len(res.Trace) != 0 {
			t.Errorf("expected empty Trace when 'trace' not requested, got %d entries", len(res.Trace))
		}
	}
}

func TestReplayBlockTransactionsUnknownTraceType(t *testing.T) {
	t.Parallel()

	api, genBlocks := replayBlockNewTestAPI(t)

	_, err := api.ReplayBlockTransactions(
		t.Context(),
		rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(genBlocks)),
		[]string{"bogus"},
	)
	if err == nil {
		t.Fatal("expected error for unknown trace type, got nil")
	}
}
