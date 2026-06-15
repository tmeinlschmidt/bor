// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
)

// Call implements the Parity/OpenEthereum trace_call RPC method: it executes a
// message against the state at the given block (without persisting it) and
// returns the requested trace outputs in ReplayResult form.
//
// Only the "trace" output is produced for now; "stateDiff" and "vmTrace" are
// accepted as valid trace types but are not yet generated (they serialize to
// null). Unknown trace types are rejected.
//
// Reverts are not Go errors: the callTracer captures the revert reason in the
// returned frame, so a reverting call still returns a ReplayResult. A genuine
// state-setup or execution-setup failure returns an error.
func (api *TraceAPI) Call(ctx context.Context, args ethapi.TransactionArgs, traceTypes []string, blockNrOrHash rpc.BlockNumberOrHash) (*ReplayResult, error) {
	set, err := parseTraceTypes(traceTypes)
	if err != nil {
		return nil, err
	}

	block, statedb, release, err := api.traceCallState(ctx, blockNrOrHash)
	if err != nil {
		return nil, err
	}
	defer release()

	result, err := api.traceCallExec(ctx, args, block, statedb, set)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// traceCallManyItem is a single entry of the trace_callMany param array. Parity
// encodes each entry as a 2-element tuple: [callObject, [traceTypes...]].
type traceCallManyItem struct {
	args       ethapi.TransactionArgs
	traceTypes []string
}

// UnmarshalJSON decodes the Parity 2-tuple [callObject, [traceTypes...]] into a
// traceCallManyItem.
func (it *traceCallManyItem) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("unmarshal trace_callMany entry: %w", err)
	}
	if len(raw) != 2 {
		return fmt.Errorf("trace_callMany entry must be a 2-element array, got %d elements", len(raw))
	}
	if err := json.Unmarshal(raw[0], &it.args); err != nil {
		return fmt.Errorf("unmarshal trace_callMany call object: %w", err)
	}
	if err := json.Unmarshal(raw[1], &it.traceTypes); err != nil {
		return fmt.Errorf("unmarshal trace_callMany trace types: %w", err)
	}
	return nil
}

// CallMany implements the Parity/OpenEthereum trace_callMany RPC method: it runs
// a batch of calls sequentially on a single shared state snapshot taken at the
// given block, so each call observes the state mutations of the calls before it
// (Parity semantics). One ReplayResult is returned per input entry.
//
// A state-setup or execution-setup failure on any entry fails the whole request.
func (api *TraceAPI) CallMany(ctx context.Context, calls []traceCallManyItem, blockNrOrHash rpc.BlockNumberOrHash) ([]*ReplayResult, error) {
	// Validate all requested trace types up front before doing any work.
	sets := make([]traceTypeSet, len(calls))
	for i := range calls {
		set, err := parseTraceTypes(calls[i].traceTypes)
		if err != nil {
			return nil, err
		}
		sets[i] = set
	}

	block, statedb, release, err := api.traceCallState(ctx, blockNrOrHash)
	if err != nil {
		return nil, err
	}
	defer release()

	results := make([]*ReplayResult, len(calls))
	for i := range calls {
		// Reuse the shared statedb so each call sees prior calls' mutations.
		result, err := api.traceCallExec(ctx, calls[i].args, block, statedb, sets[i])
		if err != nil {
			return nil, fmt.Errorf("trace_callMany entry %d: %w", i, err)
		}
		results[i] = result
	}
	return results, nil
}

// traceCallState resolves the block referenced by blockNrOrHash and returns a
// state snapshot to trace against. Pending is rejected (the same way TraceCall
// rejects it) because its contents are unstable. The caller must invoke the
// returned release function when done.
func (api *API) traceCallState(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Block, *state.StateDB, StateReleaseFunc, error) {
	var (
		block *types.Block
		err   error
	)
	if hash, ok := blockNrOrHash.Hash(); ok {
		block, err = api.blockByHash(ctx, hash)
	} else if number, ok := blockNrOrHash.Number(); ok {
		if number == rpc.PendingBlockNumber {
			return nil, nil, nil, errors.New("tracing on top of pending is not supported")
		}
		block, err = api.blockByNumber(ctx, number)
	} else {
		return nil, nil, nil, errors.New("invalid arguments; neither block nor hash specified")
	}
	if err != nil {
		return nil, nil, nil, err
	}

	statedb, release, err := api.backend.StateAtBlock(ctx, block, defaultTraceReexec, nil, true, false)
	if err != nil {
		return nil, nil, nil, err
	}
	return block, statedb, release, nil
}

// traceCallExec builds the message/transaction from args and runs it through the
// shared parityTraceTx helper, returning a ReplayResult with the requested
// outputs. It mirrors TraceCall's message-construction and basefee handling but
// does not apply state/block overrides (not used by trace_call / trace_callMany).
func (api *API) traceCallExec(ctx context.Context, args ethapi.TransactionArgs, block *types.Block, statedb *state.StateDB, set traceTypeSet) (*ReplayResult, error) {
	blockCtx := core.NewEVMBlockContext(block.Header(), api.chainContext(ctx), nil)

	if err := args.CallDefaults(api.backend.RPCGasCap(), blockCtx.BaseFee, api.backend.ChainConfig().ChainID); err != nil {
		return nil, err
	}
	msg := args.ToMessage(blockCtx.BaseFee, true)
	tx := args.ToTransaction(types.LegacyTxType)

	// Lower the basefee to 0 to avoid breaking EVM invariants (basefee < feecap).
	if msg.GasPrice.Sign() == 0 {
		blockCtx.BaseFee = new(big.Int)
	}
	if msg.BlobGasFeeCap != nil && msg.BlobGasFeeCap.BitLen() == 0 {
		blockCtx.BlobBaseFee = new(big.Int)
	}

	// trace_call / trace_callMany have no real tx/block identity, so use an empty
	// Context and strip the per-tx/block metadata from the resulting traces.
	traces, output, _, err := api.parityTraceTx(ctx, tx, msg, new(Context), blockCtx, statedb, nil, false)
	if err != nil {
		return nil, err
	}

	result := &ReplayResult{Output: output}
	if set.trace {
		result.Trace = traces
	}
	// stateDiff and vmTrace are not yet produced; leave them nil.
	return result, nil
}
