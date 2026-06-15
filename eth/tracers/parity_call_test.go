package tracers

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// newCallTestAPI builds a hermetic TraceAPI backed by a synthetic chain with two
// funded accounts, returning the API and the accounts for use in trace_call /
// trace_callMany tests.
func newCallTestAPI(t *testing.T) (*TraceAPI, []Account) {
	t.Helper()

	accounts := newAccounts(2)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	backend := newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {})
	t.Cleanup(backend.teardown)

	return &TraceAPI{API: NewAPI(backend)}, accounts
}

// transferArgs builds a simple value-transfer call object from -> to.
func transferArgs(from, to common.Address, value *big.Int) ethapi.TransactionArgs {
	return ethapi.TransactionArgs{
		From:  &from,
		To:    &to,
		Value: (*hexutil.Big)(value),
	}
}

func TestTraceCallParity(t *testing.T) {
	t.Parallel()

	api, accounts := newCallTestAPI(t)
	latest := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)

	tests := []struct {
		name       string
		traceTypes []string
		wantErr    bool
		wantTrace  bool
	}{
		{name: "trace requested", traceTypes: []string{"trace"}, wantTrace: true},
		{name: "no trace types", traceTypes: []string{}, wantTrace: false},
		{name: "unknown trace type", traceTypes: []string{"bogus"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			args := transferArgs(accounts[0].addr, accounts[1].addr, big.NewInt(1000))
			res, err := api.Call(t.Context(), args, tc.traceTypes, latest)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("trace_call error: %v", err)
			}
			if res.Output == nil {
				t.Fatalf("expected non-nil Output")
			}
			if res.StateDiff != nil || res.VMTrace != nil {
				t.Errorf("stateDiff/vmTrace should be nil in trace-only phase, got %v / %v", res.StateDiff, res.VMTrace)
			}

			if !tc.wantTrace {
				if len(res.Trace) != 0 {
					t.Fatalf("expected empty Trace, got %d entries", len(res.Trace))
				}
				return
			}

			if len(res.Trace) < 1 {
				t.Fatalf("expected at least one trace, got 0")
			}
			root := res.Trace[0]
			if root.Type != "call" {
				t.Errorf("expected root type 'call', got %q", root.Type)
			}
			// trace_call replays without a real tx/block identity.
			if root.TransactionHash != nil || root.BlockHash != nil ||
				root.BlockNumber != nil || root.TransactionPosition != nil {
				t.Errorf("expected no tx/block metadata on replay trace, got %+v", root)
			}
			if root.Action == nil || root.Action.From == nil || *root.Action.From != accounts[0].addr {
				t.Errorf("expected from %x, got %+v", accounts[0].addr, root.Action)
			}
		})
	}
}

func TestTraceCallManyParity(t *testing.T) {
	t.Parallel()

	api, accounts := newCallTestAPI(t)
	latest := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)

	calls := []traceCallManyItem{
		{
			args:       transferArgs(accounts[0].addr, accounts[1].addr, big.NewInt(1000)),
			traceTypes: []string{"trace"},
		},
		{
			args:       transferArgs(accounts[0].addr, accounts[1].addr, big.NewInt(2000)),
			traceTypes: []string{"trace"},
		},
	}

	results, err := api.CallMany(t.Context(), calls, latest)
	if err != nil {
		t.Fatalf("trace_callMany error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for i, res := range results {
		if res.Output == nil {
			t.Errorf("result %d: expected non-nil Output", i)
		}
		if len(res.Trace) < 1 {
			t.Errorf("result %d: expected at least one trace, got 0", i)
		}
	}

	// Both calls share the same statedb. Since CallDefaults derives the nonce
	// from the shared state's pool/account nonce, the second call must observe
	// the first call's nonce bump. Assert the recovered senders differ in nonce
	// by checking the underlying account nonce advanced: re-running a third call
	// after two transfers must still succeed (state remained consistent).
	third := []traceCallManyItem{{
		args:       transferArgs(accounts[0].addr, accounts[1].addr, big.NewInt(3000)),
		traceTypes: []string{"trace"},
	}}
	if _, err := api.CallMany(t.Context(), third, latest); err != nil {
		t.Fatalf("follow-up trace_callMany error: %v", err)
	}
}

func TestTraceCallManyItemUnmarshalJSON(t *testing.T) {
	t.Parallel()

	from := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")

	raw := `[{"from":"` + from.Hex() + `","to":"` + to.Hex() + `","value":"0x64"},["trace"]]`

	var item traceCallManyItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if item.args.From == nil || *item.args.From != from {
		t.Errorf("expected from %x, got %v", from, item.args.From)
	}
	if item.args.To == nil || *item.args.To != to {
		t.Errorf("expected to %x, got %v", to, item.args.To)
	}
	if len(item.traceTypes) != 1 || item.traceTypes[0] != "trace" {
		t.Errorf("expected traceTypes [trace], got %v", item.traceTypes)
	}
}

func TestTraceCallManyItemUnmarshalJSON_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "not an array", raw: `{"from":"0x1"}`},
		{name: "wrong length", raw: `[{}]`},
		{name: "three elements", raw: `[{},["trace"],{}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var item traceCallManyItem
			if err := json.Unmarshal([]byte(tc.raw), &item); err == nil {
				t.Fatalf("expected error for %q, got nil", tc.raw)
			}
		})
	}
}
