// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// parityStateDiff is the Parity/OpenEthereum "stateDiff" object: a map from
// touched account address to the per-field diff for that account.
type parityStateDiff map[common.Address]*parityAccountDiff

// parityAccountDiff holds the per-field diff for a single account. Each field is
// one of: the string "=" (unchanged), {"+": v} (created), {"-": v} (deleted) or
// {"*": {"from": x, "to": y}} (changed).
type parityAccountDiff struct {
	Balance interface{}                 `json:"balance"`
	Code    interface{}                 `json:"code"`
	Nonce   interface{}                 `json:"nonce"`
	Storage map[common.Hash]interface{} `json:"storage"`
}

// prestateAccount mirrors the JSON shape emitted by the native prestateTracer
// (see eth/tracers/native/prestate.go). Pointer fields let us distinguish an
// absent field (unchanged, in the diffMode "post" map) from a present one.
type prestateAccount struct {
	Balance *hexutil.Big                `json:"balance"`
	Nonce   *uint64                     `json:"nonce"`
	Code    *hexutil.Bytes              `json:"code"`
	Storage map[common.Hash]common.Hash `json:"storage"`
}

// prestateDiffResult mirrors the prestateTracer diffMode result {pre, post}.
type prestateDiffResult struct {
	Pre  map[common.Address]*prestateAccount `json:"pre"`
	Post map[common.Address]*prestateAccount `json:"post"`
}

func sdSame() interface{}                 { return "=" }
func sdAdded(v interface{}) interface{}   { return map[string]interface{}{"+": v} }
func sdRemoved(v interface{}) interface{} { return map[string]interface{}{"-": v} }
func sdChanged(from, to interface{}) interface{} {
	return map[string]interface{}{"*": map[string]interface{}{"from": from, "to": to}}
}

// balVal/nonceVal/codeVal return a JSON-encodable value for the field, with
// EVM-empty defaults (0 balance, 0 nonce, empty code).
func balVal(a *prestateAccount) *hexutil.Big {
	if a != nil && a.Balance != nil {
		return a.Balance
	}
	return (*hexutil.Big)(big.NewInt(0))
}

func nonceVal(a *prestateAccount) hexutil.Uint64 {
	if a != nil && a.Nonce != nil {
		return hexutil.Uint64(*a.Nonce)
	}
	return 0
}

func codeVal(a *prestateAccount) hexutil.Bytes {
	if a != nil && a.Code != nil {
		return *a.Code
	}
	return hexutil.Bytes{}
}

// isEmptyAccount reports whether the pre-state account is EVM-empty (no balance,
// nonce or code). Per EIP-161 an empty account is indistinguishable from a
// non-existent one, so such an account appearing with post-state values is
// treated as newly created.
func isEmptyAccount(a *prestateAccount) bool {
	if a == nil {
		return true
	}
	if a.Balance != nil && a.Balance.ToInt().Sign() != 0 {
		return false
	}
	if a.Nonce != nil && *a.Nonce != 0 {
		return false
	}
	if a.Code != nil && len(*a.Code) != 0 {
		return false
	}
	return len(a.Storage) == 0
}

// buildParityStateDiff converts the prestateTracer diffMode {pre, post} maps into
// the Parity stateDiff encoding.
//
// After diffMode processing the prestateTracer leaves: modified accounts in both
// pre and post (post carrying only the changed fields), deleted accounts in pre
// only, and unmodified accounts in neither. Created accounts appear in both with
// an EVM-empty pre-state.
func buildParityStateDiff(pre, post map[common.Address]*prestateAccount) parityStateDiff {
	diff := make(parityStateDiff)

	addrs := make(map[common.Address]struct{}, len(pre)+len(post))
	for a := range pre {
		addrs[a] = struct{}{}
	}
	for a := range post {
		addrs[a] = struct{}{}
	}

	for addr := range addrs {
		preAcc, postAcc := pre[addr], post[addr]
		acc := &parityAccountDiff{Storage: map[common.Hash]interface{}{}}

		switch {
		case postAcc == nil:
			// In pre only -> account was deleted. erigon's CompareStates emits only
			// balance/code/nonce "-" for a deleted account and NO storage entries
			// (storage "-" is never produced), so leave Storage empty.
			acc.Balance = sdRemoved(balVal(preAcc))
			acc.Nonce = sdRemoved(nonceVal(preAcc))
			acc.Code = sdRemoved(codeVal(preAcc))

		case isEmptyAccount(preAcc):
			// Empty pre-state with post values -> account was created.
			acc.Balance = sdAdded(balVal(postAcc))
			acc.Nonce = sdAdded(nonceVal(postAcc))
			acc.Code = sdAdded(codeVal(postAcc))
			for slot, val := range postAcc.Storage {
				acc.Storage[slot] = sdAdded(val)
			}

		default:
			// Present before and after -> per-field change (post carries only
			// the changed fields; absent field == unchanged).
			if postAcc.Balance != nil {
				acc.Balance = sdChanged(balVal(preAcc), postAcc.Balance)
			} else {
				acc.Balance = sdSame()
			}
			if postAcc.Nonce != nil {
				acc.Nonce = sdChanged(nonceVal(preAcc), hexutil.Uint64(*postAcc.Nonce))
			} else {
				acc.Nonce = sdSame()
			}
			if postAcc.Code != nil {
				acc.Code = sdChanged(codeVal(preAcc), *postAcc.Code)
			} else {
				acc.Code = sdSame()
			}
			// For an existing account, every storage change is encoded as "*";
			// a freshly written slot reads as 0 -> val and a cleared slot as
			// val -> 0 (Parity only uses "+"/"-" on created/deleted accounts).
			var zero common.Hash
			for slot, newVal := range postAcc.Storage {
				if oldVal, ok := preAcc.Storage[slot]; ok {
					acc.Storage[slot] = sdChanged(oldVal, newVal)
				} else {
					acc.Storage[slot] = sdChanged(zero, newVal)
				}
			}
			for slot, oldVal := range preAcc.Storage {
				if _, ok := postAcc.Storage[slot]; !ok {
					acc.Storage[slot] = sdChanged(oldVal, zero)
				}
			}
		}

		diff[addr] = acc
	}

	return diff
}

// parityStateDiffFor executes the message with the native prestateTracer in
// diffMode and converts the result into the Parity stateDiff encoding.
//
// preState MUST be a pre-execution copy of the state (e.g. statedb.Copy()): the
// tracer re-executes the message and advances the state it is given, so callers
// pass a throwaway copy rather than the canonical state used for trace output.
// feeless reports whether the message is a trace_call/trace_callMany simulation.
// erigon runs those with gas bailout: the gas fee is NOT debited from the sender
// (only the base-fee burn and any tip appear). We execute the full state
// transition, so when feeless we add the sender's gas debit back below.
func (api *API) parityStateDiffFor(
	ctx context.Context,
	tx *types.Transaction,
	message *core.Message,
	txctx *Context,
	vmctx vm.BlockContext,
	preState *state.StateDB,
	baseConfig *TraceConfig,
	feeless bool,
) (parityStateDiff, error) {
	prestate := "prestateTracer"
	cfg := &TraceConfig{
		Tracer:       &prestate,
		TracerConfig: json.RawMessage(`{"diffMode":true,"excludeCreatedDestroyed":true}`),
	}
	if baseConfig != nil {
		cfg.Reexec = baseConfig.Reexec
		cfg.Timeout = baseConfig.Timeout
	}

	// The native prestate tracer doesn't track the Bor base-fee recipient (burnt
	// contract), so snapshot its balance before/after the (re-)execution and add
	// the diff manually below. traceTx executes the message on preState.
	burnAddr := api.parityBurntContract(vmctx.BlockNumber.Uint64())
	var burnPre *big.Int
	if burnAddr != (common.Address{}) {
		burnPre = preState.GetBalance(burnAddr).ToBig()
	}
	// Snapshot the sender balance before execution so a feeless simulation can
	// reconstruct the erigon (no-gas-debit) sender balance afterwards.
	var senderPre *big.Int
	if feeless {
		senderPre = preState.GetBalance(message.From).ToBig()
	}

	res, usedGas, err := api.traceTx(ctx, tx, message, txctx, vmctx, preState, cfg, nil)
	if err != nil {
		return nil, err
	}

	raw, ok := res.(json.RawMessage)
	if !ok {
		if raw, err = json.Marshal(res); err != nil {
			return nil, fmt.Errorf("marshal stateDiff result: %w", err)
		}
	}

	var pd prestateDiffResult
	if err := json.Unmarshal(raw, &pd); err != nil {
		return nil, fmt.Errorf("unmarshal stateDiff result: %w", err)
	}

	sd := buildParityStateDiff(pd.Pre, pd.Post)
	if burnPre != nil {
		addBalanceOnlyDiff(sd, burnAddr, burnPre, preState.GetBalance(burnAddr).ToBig())
	}
	if feeless {
		// erigon's trace_call does not debit the sender for gas. Our execution did
		// (net debit = usedGas * effectiveGasPrice), so add it back: the corrected
		// post-balance is the actual post-balance plus the gas charge.
		gasCharge := new(big.Int).Mul(new(big.Int).SetUint64(usedGas), message.GasPrice)
		correctedPost := new(big.Int).Add(preState.GetBalance(message.From).ToBig(), gasCharge)
		setSenderBalanceDiff(sd, message.From, senderPre, correctedPost)
	}
	return sd, nil
}

// setSenderBalanceDiff overrides the sender account's balance change in the diff
// (the sender always appears because its nonce is bumped). If pre == post the
// balance becomes "=" while the rest of the account diff (nonce, etc.) is kept.
func setSenderBalanceDiff(sd parityStateDiff, addr common.Address, pre, post *big.Int) {
	acc, ok := sd[addr]
	if !ok {
		addBalanceOnlyDiff(sd, addr, pre, post)
		return
	}
	if pre.Cmp(post) == 0 {
		acc.Balance = sdSame()
		return
	}
	acc.Balance = sdChanged((*hexutil.Big)(pre), (*hexutil.Big)(post))
}

// addBalanceOnlyDiff adds a balance-only account change to the stateDiff when the
// balance changed and the account isn't already present. Used for Bor fee
// recipients that the native prestate tracer doesn't capture.
func addBalanceOnlyDiff(sd parityStateDiff, addr common.Address, pre, post *big.Int) {
	if pre.Cmp(post) == 0 {
		return
	}
	if _, ok := sd[addr]; ok {
		return
	}
	sd[addr] = &parityAccountDiff{
		Balance: sdChanged((*hexutil.Big)(pre), (*hexutil.Big)(post)),
		Code:    sdSame(),
		Nonce:   sdSame(),
		Storage: map[common.Hash]interface{}{},
	}
}
