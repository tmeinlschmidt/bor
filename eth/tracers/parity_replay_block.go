// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/rpc"
)

// ReplayBlockTransactions implements the Parity/OpenEthereum
// trace_replayBlockTransactions RPC method. It replays every transaction in the
// requested block and returns one ReplayResult per transaction, in block order.
//
// The traceTypes argument selects which trace outputs to populate (e.g.
// ["trace"]). Only the "trace" output is produced in this phase; "stateDiff"
// and "vmTrace" are accepted but always serialize to null (see ReplayResult).
//
// Unlike trace_block, the per-transaction trace entries returned here do NOT
// carry block/transaction identifier metadata, matching Parity's trace_replay*
// semantics.
func (api *TraceAPI) ReplayBlockTransactions(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash, traceTypes []string) ([]*ReplayResult, error) {
	set, err := parseTraceTypes(traceTypes)
	if err != nil {
		return nil, err
	}

	// Resolve the requested block. Hash takes precedence over number, mirroring
	// the convention used by other block-or-hash trace methods.
	var block *types.Block
	if hash, ok := blockNrOrHash.Hash(); ok {
		block, err = api.blockByHash(ctx, hash)
	} else if number, ok := blockNrOrHash.Number(); ok {
		if number == rpc.PendingBlockNumber {
			return nil, errors.New("tracing on top of pending is not supported")
		}
		block, err = api.blockByNumber(ctx, number)
	} else {
		return nil, errors.New("invalid arguments; neither block nor hash specified")
	}
	if err != nil {
		return nil, err
	}

	return api.replayBlockTransactions(ctx, block, set)
}

// replayBlockTransactions performs the per-transaction replay for a resolved
// block. It mirrors the state setup and sequential per-transaction loop of
// traceBlockParityByHash, but emits one ReplayResult per transaction (with
// trace metadata stripped) instead of a flat list of block traces.
func (api *API) replayBlockTransactions(ctx context.Context, block *types.Block, set traceTypeSet) ([]*ReplayResult, error) {
	if block.NumberU64() == 0 {
		return nil, errors.New("genesis block is not traceable")
	}

	parent, err := api.blockByNumberAndHash(ctx, rpc.BlockNumber(block.NumberU64()-1), block.ParentHash())
	if err != nil {
		return nil, fmt.Errorf("failed to get parent block: %w", err)
	}

	statedb, release, err := api.backend.StateAtBlock(ctx, parent, defaultTraceReexec, nil, true, false)
	if err != nil {
		return nil, fmt.Errorf("failed to get state at block %d: %w (archive node required for historical blocks)", parent.NumberU64(), err)
	}
	defer release()

	blockCtx := core.NewEVMBlockContext(block.Header(), api.chainContext(ctx), nil)
	evm := vm.NewEVM(blockCtx, statedb, api.backend.ChainConfig(), vm.Config{})
	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		core.ProcessBeaconBlockRoot(*beaconRoot, evm)
	}
	if api.backend.ChainConfig().IsPrague(block.Number()) {
		core.ProcessParentBlockHash(block.ParentHash(), evm)
	}

	var (
		txs               = block.Transactions()
		signer            = types.MakeSigner(api.backend.ChainConfig(), block.Number(), block.Time())
		results           = make([]*ReplayResult, 0, len(txs))
		blockHash         = block.Hash()
		cumulativeGasUsed uint64
	)

	for txIndex, tx := range txs {
		message, err := core.TransactionToMessage(tx, signer, block.BaseFee())
		if err != nil {
			return nil, fmt.Errorf("failed to convert tx to message (tx %d): %w", txIndex, err)
		}

		txctx := &Context{
			BlockHash:         blockHash,
			BlockNumber:       block.Number(),
			TxIndex:           txIndex,
			TxHash:            tx.Hash(),
			CumulativeGasUsed: cumulativeGasUsed,
			LogIndex:          len(statedb.Logs()),
		}

		result := &ReplayResult{}

		// stateDiff is computed on a pre-tx copy, since the trace run below
		// advances statedb for the following transactions.
		if set.stateDiff {
			sd, err := api.parityStateDiffFor(ctx, tx, message, txctx, blockCtx, statedb.Copy(), nil)
			if err != nil {
				return nil, fmt.Errorf("failed to build stateDiff for tx %d: %w", txIndex, err)
			}
			result.StateDiff = sd
		}

		// includeTxMeta is false: trace_replay* entries omit block/tx identifiers.
		traces, output, gasUsed, err := api.parityTraceTx(ctx, tx, message, txctx, blockCtx, statedb, nil, false)
		if err != nil {
			return nil, fmt.Errorf("failed to trace tx %d: %w", txIndex, err)
		}
		cumulativeGasUsed += gasUsed

		result.Output = output
		if set.trace {
			result.Trace = traces
		}
		// VMTrace is intentionally left nil: not produced in this phase.
		results = append(results, result)
	}

	return results, nil
}
