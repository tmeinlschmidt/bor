// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"encoding/json"

	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/params"
)

// parityCallTracerName is the registered tracer backing the Parity trace
// conversion. It wraps the native callTracer and additionally captures the
// transaction's total gas refund so the root trace's gasUsed can report the
// gross execution gas (refund excluded), matching erigon.
const parityCallTracerName = "parityCallTracer"

func init() {
	DefaultDirectory.Register(parityCallTracerName, newParityCallTracer, false)
}

// parityCallResult is the JSON returned by the wrapper: the underlying
// callTracer frame plus the captured refund.
type parityCallResult struct {
	Frame  json.RawMessage `json:"frame"`
	Refund uint64          `json:"refund"`
}

// parityCallTracer wraps the native callTracer and records the EVM gas refund.
type parityCallTracer struct {
	inner  *Tracer
	refund uint64
}

// newParityCallTracer builds the wrapper around the native callTracer. The
// TracerConfig is forwarded to the inner callTracer unchanged.
func newParityCallTracer(ctx *Context, cfg json.RawMessage, chainConfig *params.ChainConfig) (*Tracer, error) {
	inner, err := DefaultDirectory.New("callTracer", ctx, cfg, chainConfig)
	if err != nil {
		return nil, err
	}
	t := &parityCallTracer{inner: inner}

	// Copy the inner hooks and compose an OnGasChange that captures the tx refund
	// (GasChangeTxRefunds is fired once, as old -> old+refund).
	hooks := *inner.Hooks
	innerOnGasChange := hooks.OnGasChange
	hooks.OnGasChange = func(old, new uint64, reason tracing.GasChangeReason) {
		if reason == tracing.GasChangeTxRefunds && new > old {
			t.refund += new - old
		}
		if innerOnGasChange != nil {
			innerOnGasChange(old, new, reason)
		}
	}

	return &Tracer{
		Hooks:     &hooks,
		GetResult: t.getResult,
		Stop:      inner.Stop,
	}, nil
}

// getResult returns the inner callTracer frame together with the captured refund.
func (t *parityCallTracer) getResult() (json.RawMessage, error) {
	frame, err := t.inner.GetResult()
	if err != nil {
		return nil, err
	}
	return json.Marshal(parityCallResult{Frame: frame, Refund: t.refund})
}
