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
	opcode   vm.OpCode // the executed opcode (for push count / mem region)
	gasStart uint64
	preStack []uint256.Int // copy of pre-op stack (bottom-first) for mem operands
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

	// Finalize the previous op now that the scope reflects its result. This op's
	// gas == the gas remaining after the previous op (including gas returned by a
	// sub-call), which is exactly Parity's ex.used for that previous op.
	if cur.pending != nil {
		t.finalizeWithScope(cur.pending, scope, gas)
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

	// Pre-op snapshot: opcode (push count / mem region) + a copy of the stack
	// (memory-write operands). push/mem are finalized at the next OnOpcode.
	pre := &vmTracePending{
		op:       entry,
		opcode:   op,
		gasStart: gas,
		preStack: append([]uint256.Int{}, scope.StackData()...),
	}
	entry.Ex = &vmTraceEx{Push: []string{}, Used: gas - cost}
	if store != nil {
		entry.Ex.Store = store
	}
	cur.pending = pre
}

// finalizeWithScope completes an op's ex (push, mem, used) using the op's known
// stack-push count and memory-write region read against the post-op scope, and
// nextGas = the gas remaining after the op (this is Parity's ex.used, correct
// for calls where the callee returns leftover gas).
func (t *parityVMTracer) finalizeWithScope(p *vmTracePending, scope tracing.OpContext, nextGas uint64) {
	p.op.Ex.Used = nextGas
	// push = the op's pushed value(s), i.e. the top N items of the post-op stack.
	if n := vmTraceOpPushCount(p.opcode); n > 0 {
		curStack := scope.StackData()
		if n > len(curStack) {
			n = len(curStack)
		}
		push := make([]string, 0, n)
		for i := len(curStack) - n; i < len(curStack); i++ {
			push = append(push, hexutil.EncodeBig(curStack[i].ToBig()))
		}
		p.op.Ex.Push = push
	}
	// mem = the exact region the op wrote, read from post-op memory.
	if off, size, writes := vmTraceMemRegion(p.opcode, p.preStack); writes {
		mem := scope.MemoryData()
		if off+size <= uint64(len(mem)) {
			p.op.Ex.Mem = &vmTraceMem{Off: int(off), Data: append(hexutil.Bytes{}, mem[off:off+size]...)}
		}
	}
}

// finalizeNoScope completes the last op of a frame at OnExit, where no scope is
// available. Terminal ops push nothing and write no memory, so leaving push/mem
// empty is correct.
func (t *parityVMTracer) finalizeNoScope(p *vmTracePending) {
	if p.op.Ex == nil {
		p.op.Ex = &vmTraceEx{Push: []string{}, Used: p.gasStart - p.op.Cost}
	}
}

// vmTraceOpPushCount returns how many words the opcode pushes onto the stack
// (the count Parity's vmTrace reports in "push": the items on top of the post-op
// stack). Derived from the core/vm jump-table push counts.
func vmTraceOpPushCount(op vm.OpCode) int {
	switch op {
	case vm.ADD, vm.MUL, vm.SUB, vm.DIV, vm.SDIV, vm.MOD, vm.SMOD,
		vm.ADDMOD, vm.MULMOD, vm.EXP, vm.SIGNEXTEND:
		return 1
	case vm.LT, vm.GT, vm.SLT, vm.SGT, vm.EQ, vm.ISZERO,
		vm.AND, vm.OR, vm.XOR, vm.NOT, vm.BYTE,
		vm.SHL, vm.SHR, vm.SAR, vm.CLZ:
		return 1
	case vm.KECCAK256:
		return 1
	case vm.ADDRESS, vm.BALANCE, vm.ORIGIN, vm.CALLER, vm.CALLVALUE,
		vm.CALLDATALOAD, vm.CALLDATASIZE, vm.CODESIZE, vm.GASPRICE,
		vm.EXTCODESIZE, vm.RETURNDATASIZE, vm.EXTCODEHASH:
		return 1
	case vm.BLOCKHASH, vm.COINBASE, vm.TIMESTAMP, vm.NUMBER,
		vm.DIFFICULTY, vm.GASLIMIT,
		vm.CHAINID, vm.SELFBALANCE, vm.BASEFEE, vm.BLOBHASH, vm.BLOBBASEFEE:
		return 1
	case vm.MLOAD, vm.SLOAD, vm.TLOAD, vm.PC, vm.MSIZE, vm.GAS:
		return 1
	case vm.CREATE, vm.CREATE2, vm.CALL, vm.CALLCODE,
		vm.DELEGATECALL, vm.STATICCALL:
		return 1
	case vm.STOP, vm.POP, vm.MSTORE, vm.MSTORE8, vm.SSTORE, vm.TSTORE,
		vm.JUMP, vm.JUMPI, vm.JUMPDEST, vm.MCOPY,
		vm.RETURN, vm.REVERT, vm.INVALID, vm.SELFDESTRUCT:
		return 0
	case vm.CALLDATACOPY, vm.CODECOPY, vm.EXTCODECOPY, vm.RETURNDATACOPY:
		return 0
	}
	switch {
	case op >= vm.PUSH0 && op <= vm.PUSH32:
		return 1
	// Parity/OpenEthereum reports the top `ret` items, and for DUPn/SWAPn ret = n+1
	// (DUP1 -> 2 copies of the value, SWAP1 -> the 2 swapped values, etc.), not the
	// net stack growth.
	case op >= vm.DUP1 && op <= vm.DUP16:
		return int(op-vm.DUP1) + 2
	case op >= vm.SWAP1 && op <= vm.SWAP16:
		return int(op-vm.SWAP1) + 2
	case op >= vm.LOG0 && op <= vm.LOG4:
		return 0
	}
	return 0
}

// vmTraceMemRegion returns the memory region [off, off+size) written by the
// opcode, derived from the pre-op stack (bottom-first, as scope.StackData()).
// writes=false if the opcode doesn't write memory or the length is zero. Stack
// arg order verified against core/vm/instructions.go.
func vmTraceMemRegion(op vm.OpCode, stack []uint256.Int) (off uint64, size uint64, writes bool) {
	top := func(n int) (uint64, bool) {
		idx := len(stack) - n
		if idx < 0 {
			return 0, false
		}
		val := stack[idx]
		if !val.IsUint64() {
			return 0, false
		}
		return val.Uint64(), true
	}
	switch op {
	case vm.MSTORE:
		if o, ok := top(1); ok {
			return o, 32, true
		}
	case vm.MSTORE8:
		if o, ok := top(1); ok {
			return o, 1, true
		}
	case vm.CALLDATACOPY, vm.CODECOPY, vm.RETURNDATACOPY, vm.MCOPY:
		o, ok1 := top(1)
		l, ok3 := top(3)
		if ok1 && ok3 && l != 0 {
			return o, l, true
		}
	case vm.EXTCODECOPY:
		o, ok2 := top(2)
		l, ok4 := top(4)
		if ok2 && ok4 && l != 0 {
			return o, l, true
		}
	}
	return 0, 0, false
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
