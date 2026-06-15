// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
)

// ReplayResult is the result wrapper returned by the Parity/OpenEthereum
// trace_call, trace_callMany, trace_replayTransaction and
// trace_replayBlockTransactions methods.
//
// The Output field is always present. The Trace, StateDiff and VMTrace fields
// are populated only when the corresponding trace type is requested via the
// traceTypes argument; otherwise they serialize to null.
//
// NOTE: stateDiff and vmTrace are not yet produced (tracked in
// fixes_docs/FEATURE-trace-methods-plan). They always serialize to null for now.
type ReplayResult struct {
	Output    *hexutil.Bytes `json:"output"`
	StateDiff interface{}    `json:"stateDiff"`
	Trace     []*ParityTrace `json:"trace"`
	VMTrace   interface{}    `json:"vmTrace"`
}

// traceTypeSet captures which Parity trace outputs the caller requested.
type traceTypeSet struct {
	trace     bool
	stateDiff bool
	vmTrace   bool
}

// parseTraceTypes validates and parses the Parity traceTypes argument
// (e.g. ["trace", "stateDiff", "vmTrace"]). Unknown types are rejected.
func parseTraceTypes(types []string) (traceTypeSet, error) {
	var set traceTypeSet
	for _, t := range types {
		switch t {
		case "trace":
			set.trace = true
		case "stateDiff":
			set.stateDiff = true
		case "vmTrace":
			set.vmTrace = true
		default:
			return set, fmt.Errorf("unknown trace type: %q", t)
		}
	}
	return set, nil
}

// stripTxMeta clears the per-transaction/block identifier fields from trace
// entries. Parity's trace_call / trace_replay* responses omit these fields,
// whereas trace_block / trace_transaction include them.
func stripTxMeta(traces []*ParityTrace) {
	for _, t := range traces {
		t.BlockHash = nil
		t.BlockNumber = nil
		t.TransactionHash = nil
		t.TransactionPosition = nil
	}
}

// parityTraceTx executes a single message with the callTracer and flattens the
// resulting call frame into Parity traces. It also returns the top-level call
// output and the gas used by the execution.
//
// The block/transaction identifier fields on the returned traces are populated
// from txctx; when includeTxMeta is false they are cleared (trace_call /
// trace_replay* semantics). The callTracer is always forced regardless of any
// tracer set on baseConfig, since the Parity conversion requires a structured
// call frame; only Reexec/Timeout are honoured from baseConfig.
func (api *API) parityTraceTx(
	ctx context.Context,
	tx *types.Transaction,
	message *core.Message,
	txctx *Context,
	vmctx vm.BlockContext,
	statedb *state.StateDB,
	baseConfig *TraceConfig,
	includeTxMeta bool,
) ([]*ParityTrace, *hexutil.Bytes, uint64, error) {
	callTracer := "callTracer"
	traceConfig := &TraceConfig{
		Tracer:       &callTracer,
		TracerConfig: json.RawMessage(`{}`),
	}
	if baseConfig != nil {
		traceConfig.Reexec = baseConfig.Reexec
		traceConfig.Timeout = baseConfig.Timeout
	}

	res, gasUsed, err := api.traceTx(ctx, tx, message, txctx, vmctx, statedb, traceConfig, nil)
	if err != nil {
		return nil, nil, 0, err
	}

	// The callTracer returns json.RawMessage; marshal defensively otherwise.
	raw, ok := res.(json.RawMessage)
	if !ok {
		if raw, err = json.Marshal(res); err != nil {
			return nil, nil, 0, fmt.Errorf("marshal trace result: %w", err)
		}
	}

	var callFrame map[string]interface{}
	if err := json.Unmarshal(raw, &callFrame); err != nil {
		return nil, nil, 0, fmt.Errorf("unmarshal trace result: %w", err)
	}

	var (
		blockHash   common.Hash
		blockNumber uint64
		txHash      common.Hash
		txIndex     uint64
	)
	if txctx != nil {
		blockHash = txctx.BlockHash
		if txctx.BlockNumber != nil {
			blockNumber = txctx.BlockNumber.Uint64()
		}
		txHash = txctx.TxHash
		txIndex = uint64(txctx.TxIndex)
	}

	traces, err := convertCallFrameToParityTraces(callFrame, []uint64{}, txHash, txIndex, blockHash, blockNumber, tx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("convert trace: %w", err)
	}
	if !includeTxMeta {
		stripTxMeta(traces)
	}

	return traces, parityOutput(callFrame), gasUsed, nil
}

// parityOutput extracts the top-level call output as hex bytes, defaulting to
// an empty byte slice (which serializes to "0x").
func parityOutput(callFrame map[string]interface{}) *hexutil.Bytes {
	out := hexutil.Bytes{}
	if o, ok := callFrame["output"].(string); ok && o != "" {
		if b, err := hexutil.Decode(o); err == nil {
			out = b
		}
	}
	return &out
}

// Transaction implements the trace_transaction RPC method: it returns the
// Parity-format traces for a single mined transaction identified by its hash.
func (api *TraceAPI) Transaction(ctx context.Context, txHash common.Hash) ([]*ParityTrace, error) {
	return api.transactionParity(ctx, txHash, nil)
}

// transactionParity is the internal implementation backing trace_transaction.
func (api *API) transactionParity(ctx context.Context, hash common.Hash, config *TraceConfig) ([]*ParityTrace, error) {
	found, _, blockHash, blockNumber, index := api.backend.GetCanonicalTransaction(hash)
	if !found {
		// Warn in case tx indexer is not done.
		if !api.backend.TxIndexDone() {
			return nil, ethapi.NewTxIndexingError()
		}
		// Only mined txes are supported.
		return nil, errTxNotFound
	}
	if blockNumber == 0 {
		return nil, errors.New("genesis is not traceable")
	}

	reexec := defaultTraceReexec
	if config != nil && config.Reexec != nil {
		reexec = *config.Reexec
	}

	block, err := api.blockByNumberAndHash(ctx, rpc.BlockNumber(blockNumber), blockHash)
	if err != nil {
		return nil, err
	}

	tx, vmctx, statedb, release, err := api.backend.StateAtTransaction(ctx, block, int(index), reexec)
	if err != nil {
		return nil, err
	}
	defer release()

	txctx := &Context{
		BlockHash:   blockHash,
		BlockNumber: block.Number(),
		TxIndex:     int(index),
		TxHash:      hash,
		// CumulativeGasUsed is only consulted for Bor state-sync transactions,
		// which are always the last tx in a block; use the block's total gas.
		CumulativeGasUsed: block.GasUsed(),
		LogIndex:          len(statedb.Logs()),
	}

	msg, err := core.TransactionToMessage(tx, types.MakeSigner(api.backend.ChainConfig(), block.Number(), block.Time()), block.BaseFee())
	if err != nil {
		return nil, err
	}

	traces, _, _, err := api.parityTraceTx(ctx, tx, msg, txctx, vmctx, statedb, config, true)
	return traces, err
}
