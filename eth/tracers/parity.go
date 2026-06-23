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
type ReplayResult struct {
	Output    *hexutil.Bytes `json:"output"`
	StateDiff interface{}    `json:"stateDiff"`
	Trace     []*ParityTrace `json:"trace"`
	VMTrace   interface{}    `json:"vmTrace"`
	// TransactionHash is set by trace_replayTransaction / trace_replayBlockTransactions
	// (which replay a real, mined tx) and omitted by trace_call / trace_callMany.
	TransactionHash *common.Hash `json:"transactionHash,omitempty"`
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
	// parityCallTracer wraps the native callTracer and also reports the tx gas
	// refund, which the root trace needs to report gross gasUsed (erigon semantics).
	tracerName := parityCallTracerName
	traceConfig := &TraceConfig{
		Tracer:       &tracerName,
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

	// The tracer returns json.RawMessage; marshal defensively otherwise.
	raw, ok := res.(json.RawMessage)
	if !ok {
		if raw, err = json.Marshal(res); err != nil {
			return nil, nil, 0, fmt.Errorf("marshal trace result: %w", err)
		}
	}

	var wrapped parityCallResult
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, nil, 0, fmt.Errorf("unmarshal trace result: %w", err)
	}

	var callFrame map[string]interface{}
	if err := json.Unmarshal(wrapped.Frame, &callFrame); err != nil {
		return nil, nil, 0, fmt.Errorf("unmarshal call frame: %w", err)
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

	// Compute the exact transaction intrinsic gas so the root trace's gas/gasUsed
	// exclude it (Parity/erigon semantics). Using core.IntrinsicGas handles access
	// lists, auth lists and the relevant EIPs precisely.
	var intrinsicGas uint64
	if message != nil {
		rules := api.backend.ChainConfig().Rules(vmctx.BlockNumber, vmctx.Random != nil, vmctx.Time)
		if ig, ierr := core.IntrinsicGas(message.Data, message.AccessList, message.SetCodeAuthorizations, message.To == nil, rules.IsHomestead, rules.IsIstanbul, rules.IsShanghai); ierr == nil {
			intrinsicGas = ig
		}
	}

	// Root gasUsed = gross EVM execution gas = gasLimit - postExecGasRemaining -
	// intrinsic. This excludes the intrinsic cost, the EIP-7623 data floor and gas
	// refunds, matching erigon. Falls back to the callTracer's (net) gasUsed if the
	// post-execution gas wasn't observed.
	var rootGasUsed uint64
	if message != nil && wrapped.GasLeftSet && message.GasLimit >= wrapped.GasLeft+intrinsicGas {
		rootGasUsed = message.GasLimit - wrapped.GasLeft - intrinsicGas
	} else if s, ok := callFrame["gasUsed"].(string); ok {
		if gu, derr := hexutil.DecodeUint64(s); derr == nil && gu >= intrinsicGas {
			rootGasUsed = gu - intrinsicGas
		}
	}

	traces, err := convertCallFrameToParityTraces(callFrame, []uint64{}, txHash, txIndex, blockHash, blockNumber, intrinsicGas, rootGasUsed)
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
	// Bor zeroes header.Coinbase; use the consensus author so COINBASE/fees resolve.
	vmctx.Coinbase = api.parityBlockAuthor(block.Header())

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
