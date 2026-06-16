// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sync/atomic"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// vmTraceName is the registered tracer name backing the Parity/OpenEthereum
// "vmTrace" trace type.
const vmTraceName = "parityVmTracer"

func init() {
	DefaultDirectory.Register(vmTraceName, newParityVMTracer, false)
}

// vmTraceFrame is the Parity vmTrace object for a single call frame:
//
//	{"code": "0x<executed bytecode>", "ops": [op...]}
type vmTraceFrame struct {
	Code hexutil.Bytes `json:"code"`
	Ops  []*vmTraceOp  `json:"ops"`
}

// vmTraceOp is a single executed opcode entry within a frame.
type vmTraceOp struct {
	PC   uint64        `json:"pc"`
	Cost uint64        `json:"cost"`
	Op   string        `json:"op"`
	Idx  string        `json:"idx"`
	Ex   *vmTraceEx    `json:"ex"`
	Sub  *vmTraceFrame `json:"sub"`
}

// vmTraceEx captures the post-execution effect of an op: memory written, values
// pushed, storage written and gas remaining.
type vmTraceEx struct {
	Mem   *vmTraceMem `json:"mem"`
	Push  []string    `json:"push"`
	Store *vmTraceSto `json:"store"`
	Used  uint64      `json:"used"`
}

// vmTraceMem is a contiguous memory write: offset + written bytes.
type vmTraceMem struct {
	Off  int           `json:"off"`
	Data hexutil.Bytes `json:"data"`
}

// vmTraceSto is an SSTORE: the key/value written (each 32-byte hex).
type vmTraceSto struct {
	Key common.Hash `json:"key"`
	Val common.Hash `json:"val"`
}

// vmTracePending holds the still-open op of a frame whose post-op effects
// (push/mem) can only be observed at the NEXT OnOpcode (look-ahead).
type vmTracePending struct {
	op       *vmTraceOp
	gasStart uint64
	preStack []byte // copy of pre-op stack words (bottom-first, 32 bytes each)
	preMem   []byte // copy of pre-op memory
	preLen   int    // pre-op stack length (number of words)
}

// vmTraceState is the per-frame bookkeeping kept on a stack mirroring the EVM
// call stack.
type vmTraceState struct {
	frame   *vmTraceFrame
	prefix  string          // idx prefix for ops in this frame ("0", "0-160", ...)
	next    int             // next op index within this frame
	pending *vmTracePending // op awaiting finalization at the next step / exit
}

// parityVMTracer implements the Parity/OpenEthereum vmTrace (opcode-level trace).
type parityVMTracer struct {
	statedb   tracing.StateDB
	root      *vmTraceFrame
	stack     []*vmTraceState // index 0 == root frame
	interrupt atomic.Bool
	reason    error
}

// newParityVMTracer constructs the vmTrace tracer.
func newParityVMTracer(_ *Context, _ json.RawMessage, _ *params.ChainConfig) (*Tracer, error) {
	t := &parityVMTracer{}
	return &Tracer{
		Hooks: &tracing.Hooks{
			OnTxStart: t.OnTxStart,
			OnTxEnd:   t.OnTxEnd,
			OnEnter:   t.OnEnter,
			OnExit:    t.OnExit,
			OnOpcode:  t.OnOpcode,
		},
		GetResult: t.GetResult,
		Stop:      t.Stop,
	}, nil
}

// OnTxStart captures the state DB used to resolve frame code for call frames.
func (t *parityVMTracer) OnTxStart(env *tracing.VMContext, _ *types.Transaction, _ common.Address) {
	t.statedb = env.StateDB
}

// OnTxEnd is a no-op placeholder kept for symmetry with other tracers.
func (t *parityVMTracer) OnTxEnd(_ *types.Receipt, _ error) {}

// OnEnter is invoked when a new message (call/create) starts. The root frame is
// established at depth 0; deeper frames are pushed as the .sub of the parent's
// current pending op.
func (t *parityVMTracer) OnEnter(depth int, typ byte, _ common.Address, to common.Address, input []byte, _ uint64, _ *big.Int) {
	if t.interrupt.Load() {
		return
	}
	op := vm.OpCode(typ)

	frame := &vmTraceFrame{Code: hexutil.Bytes{}, Ops: []*vmTraceOp{}}
	switch op {
	case vm.CREATE, vm.CREATE2:
		frame.Code = append(hexutil.Bytes{}, input...)
	default:
		if t.statedb != nil {
			frame.Code = append(hexutil.Bytes{}, t.statedb.GetCode(to)...)
		}
	}

	if depth == 0 || len(t.stack) == 0 {
		t.root = frame
		t.stack = []*vmTraceState{{frame: frame, prefix: "0"}}
		return
	}

	parent := t.stack[len(t.stack)-1]
	prefix := "0"
	if parent.pending != nil {
		// Attach the sub-frame to the call op that opened it.
		parent.pending.op.Sub = frame
		prefix = parent.pending.op.Idx
	}
	t.stack = append(t.stack, &vmTraceState{frame: frame, prefix: prefix})
}

// OnExit pops the current frame, finalizing its last pending op (no scope is
// available, so push/mem are empty — acceptable for terminal STOP/RETURN/REVERT).
func (t *parityVMTracer) OnExit(_ int, _ []byte, _ uint64, _ error, _ bool) {
	if t.interrupt.Load() {
		return
	}
	if len(t.stack) == 0 {
		return
	}
	cur := t.stack[len(t.stack)-1]
	if cur.pending != nil {
		t.finalizeNoScope(cur.pending)
		cur.pending = nil
	}
	t.stack = t.stack[:len(t.stack)-1]
}

// OnOpcode records a new op for the current frame and, via look-ahead, finalizes
// the previous pending op of that frame using the current (post-previous-op) scope.
func (t *parityVMTracer) OnOpcode(pc uint64, opcode byte, gas, cost uint64, scope tracing.OpContext, _ []byte, _ int, err error) {
	if err != nil || t.interrupt.Load() {
		return
	}
	if len(t.stack) == 0 {
		return
	}
	cur := t.stack[len(t.stack)-1]

	// Finalize the previous op now that the scope reflects its result.
	if cur.pending != nil {
		t.finalizeWithScope(cur.pending, scope)
		cur.pending = nil
	}

	op := vm.OpCode(opcode)
	entry := &vmTraceOp{
		PC:   pc,
		Cost: cost,
		Op:   op.String(),
		Idx:  fmt.Sprintf("%s-%d", cur.prefix, cur.next),
	}
	cur.next++
	cur.frame.Ops = append(cur.frame.Ops, entry)

	// SSTORE store value is available now (operands still on the stack).
	var store *vmTraceSto
	if op == vm.SSTORE {
		data := scope.StackData()
		if n := len(data); n >= 2 {
			key := common.Hash(data[n-1].Bytes32())
			val := common.Hash(data[n-2].Bytes32())
			store = &vmTraceSto{Key: key, Val: val}
		}
	}

	// Pre-op snapshots used to diff push/mem at the next step.
	stackData := scope.StackData()
	pre := &vmTracePending{
		op:       entry,
		gasStart: gas,
		preStack: vmTraceStackBytes(stackData),
		preMem:   append([]byte{}, scope.MemoryData()...),
		preLen:   len(stackData),
	}
	entry.Ex = &vmTraceEx{Push: []string{}, Used: gas - cost}
	if store != nil {
		entry.Ex.Store = store
	}
	cur.pending = pre
}

// finalizeWithScope completes an op's ex.push and ex.mem by diffing the pre-op
// snapshot against the current scope.
func (t *parityVMTracer) finalizeWithScope(p *vmTracePending, scope tracing.OpContext) {
	curStack := scope.StackData()
	p.op.Ex.Push = vmTracePushDiff(p.preStack, p.preLen, curStack)
	p.op.Ex.Mem = vmTraceMemDiff(p.preMem, scope.MemoryData())
}

// finalizeNoScope completes the last op of a frame at OnExit, where no scope is
// available. Terminal ops push nothing and write no memory, so leaving push/mem
// empty is correct.
func (t *parityVMTracer) finalizeNoScope(p *vmTracePending) {
	if p.op.Ex == nil {
		p.op.Ex = &vmTraceEx{Push: []string{}, Used: p.gasStart - p.op.Cost}
	}
}

// vmTraceStackBytes flattens the uint256 stack (bottom-first) into a byte slice
// of 32-byte words for cheap comparison.
func vmTraceStackBytes(data []uint256.Int) []byte {
	out := make([]byte, 0, len(data)*32)
	for i := range data {
		b := data[i].Bytes32()
		out = append(out, b[:]...)
	}
	return out
}

// vmTracePushDiff returns the values present in the current stack above the
// longest common prefix shared with the pre-op stack (compared from the bottom).
// This captures PUSH/DUP/arithmetic results. SWAP may over-report (documented v1
// limitation) since it rewrites items below the top.
func vmTracePushDiff(preStack []byte, preLen int, cur []uint256.Int) []string {
	// Common prefix length in words, counted from the bottom of the stack.
	commonWords := 0
	maxWords := preLen
	if len(cur) < maxWords {
		maxWords = len(cur)
	}
	for commonWords < maxWords {
		off := commonWords * 32
		b := cur[commonWords].Bytes32()
		if !bytesEqual32(preStack[off:off+32], b[:]) {
			break
		}
		commonWords++
	}
	push := make([]string, 0, len(cur)-commonWords)
	for i := commonWords; i < len(cur); i++ {
		v := cur[i]
		push = append(push, hexutil.EncodeBig(v.ToBig()))
	}
	return push
}

// bytesEqual32 compares two 32-byte slices.
func bytesEqual32(a, b []byte) bool {
	for i := 0; i < 32; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// vmTraceMemDiff reports the contiguous changed byte range between pre-op and
// post-op memory, or nil if memory is unchanged.
func vmTraceMemDiff(pre, cur []byte) *vmTraceMem {
	if len(cur) == 0 {
		return nil
	}
	first := -1
	last := -1
	for i := 0; i < len(cur); i++ {
		var pb byte
		if i < len(pre) {
			pb = pre[i]
		}
		if cur[i] != pb {
			if first == -1 {
				first = i
			}
			last = i
		}
	}
	if first == -1 {
		return nil
	}
	// EVM memory writes operate on full 32-byte words (MSTORE etc.), and erigon
	// reports the whole written word even when some bytes coincide with prior
	// (zero) memory. Round the changed range out to word boundaries so a write
	// like MSTORE(0x80) at offset 64 reports off=64 with the full 32-byte word
	// rather than just the single non-zero byte.
	start := (first / 32) * 32
	end := ((last / 32) + 1) * 32
	if end > len(cur) {
		end = len(cur)
	}
	return &vmTraceMem{Off: start, Data: append(hexutil.Bytes{}, cur[start:end]...)}
}

// GetResult marshals the root frame as the vmTrace object. For a plain value
// transfer to an EOA (no code, no ops) it yields {"code":"0x","ops":[]}.
func (t *parityVMTracer) GetResult() (json.RawMessage, error) {
	frame := t.root
	if frame == nil {
		frame = &vmTraceFrame{Code: hexutil.Bytes{}, Ops: []*vmTraceOp{}}
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("marshal vmTrace: %w", err)
	}
	return raw, t.reason
}

// Stop terminates tracing at the next opportunity.
func (t *parityVMTracer) Stop(err error) {
	t.reason = err
	t.interrupt.Store(true)
}

// parityVMTraceFor executes the message with the vmTrace tracer and returns the
// raw {code, ops} object.
//
// preState MUST be a pre-execution copy of the state (e.g. statedb.Copy()): the
// tracer re-executes the message and advances the state it is given.
func (api *API) parityVMTraceFor(
	ctx context.Context,
	tx *types.Transaction,
	message *core.Message,
	txctx *Context,
	vmctx vm.BlockContext,
	preState *state.StateDB,
	baseConfig *TraceConfig,
) (json.RawMessage, error) {
	name := vmTraceName
	cfg := &TraceConfig{Tracer: &name}
	if baseConfig != nil {
		cfg.Reexec = baseConfig.Reexec
		cfg.Timeout = baseConfig.Timeout
	}

	res, _, err := api.traceTx(ctx, tx, message, txctx, vmctx, preState, cfg, nil)
	if err != nil {
		return nil, err
	}

	raw, ok := res.(json.RawMessage)
	if !ok {
		if raw, err = json.Marshal(res); err != nil {
			return nil, fmt.Errorf("marshal vmTrace result: %w", err)
		}
	}
	return raw, nil
}
