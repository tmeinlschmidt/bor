package tracers

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
)

// ReplayTransaction implements the trace_replayTransaction RPC method. It
// replays a single mined transaction and returns a ReplayResult whose contents
// are gated by traceTypes (e.g. ["trace"]).
//
// NOTE: stateDiff and vmTrace are not yet produced; requesting them yields a
// null field (tracked in fixes_docs/FEATURE-trace-methods-plan).
func (api *TraceAPI) ReplayTransaction(ctx context.Context, txHash common.Hash, traceTypes []string) (*ReplayResult, error) {
	set, err := parseTraceTypes(traceTypes)
	if err != nil {
		return nil, err
	}

	found, _, blockHash, blockNumber, index := api.backend.GetCanonicalTransaction(txHash)
	if !found {
		if !api.backend.TxIndexDone() {
			return nil, ethapi.NewTxIndexingError()
		}
		return nil, errTxNotFound
	}
	if blockNumber == 0 {
		return nil, errors.New("genesis is not traceable")
	}

	block, err := api.blockByNumberAndHash(ctx, rpc.BlockNumber(blockNumber), blockHash)
	if err != nil {
		return nil, err
	}

	tx, vmctx, statedb, release, err := api.backend.StateAtTransaction(ctx, block, int(index), defaultTraceReexec)
	if err != nil {
		return nil, err
	}
	defer release()

	txctx := &Context{
		BlockHash:   blockHash,
		BlockNumber: block.Number(),
		TxIndex:     int(index),
		TxHash:      txHash,
		// Only consulted for Bor state-sync txs, which are always last in a block.
		CumulativeGasUsed: block.GasUsed(),
		LogIndex:          len(statedb.Logs()),
	}

	msg, err := core.TransactionToMessage(tx, types.MakeSigner(api.backend.ChainConfig(), block.Number(), block.Time()), block.BaseFee())
	if err != nil {
		return nil, err
	}

	// trace_replayTransaction omits per-tx/block identifiers on each entry.
	traces, output, _, err := api.parityTraceTx(ctx, tx, msg, txctx, vmctx, statedb, nil, false)
	if err != nil {
		return nil, err
	}

	result := &ReplayResult{Output: output}
	if set.trace {
		result.Trace = traces
	}
	// StateDiff / VMTrace remain nil until those trace types are implemented.
	return result, nil
}
