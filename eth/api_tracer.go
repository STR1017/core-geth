// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params/types/ctypes"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	// defaultTraceTimeout is the amount of time a single transaction can execute
	// by default before being forcefully aborted.
	defaultTraceTimeout = 5 * time.Second

	// defaultTraceReexec is the number of blocks the tracer is willing to go back
	// and reexecute to produce missing historical state necessary to run a specific
	// trace.
	defaultTraceReexec = uint64(128)
)

// TraceConfig holds extra parameters to trace functions.
type TraceConfig struct {
	*vm.LogConfig
	Tracer            *string
	Timeout           *string
	Reexec            *uint64
	NestedTraceOutput bool // Returns the trace output JSON nested under the trace name key. This allows full Parity compatibility to be achieved.
}

// StdTraceConfig holds extra parameters to standard-json trace functions.
type StdTraceConfig struct {
	*vm.LogConfig
	Reexec *uint64
	TxHash common.Hash
}

// txTraceResult is the result of a single transaction trace.
type txTraceResult struct {
	Result interface{} `json:"result,omitempty"` // Trace results produced by the tracer
	Error  string      `json:"error,omitempty"`  // Trace failure produced by the tracer
}

// blockTraceTask represents a single block trace task when an entire chain is
// being traced.
type blockTraceTask struct {
	statedb *state.StateDB   // Intermediate state prepped for tracing
	block   *types.Block     // Block to trace the transactions from
	rootref common.Hash      // Trie root reference held for this task
	results []*txTraceResult // Trace results procudes by the task
}

// blockTraceResult represets the results of tracing a single block when an entire
// chain is being traced.
type blockTraceResult struct {
	Block  hexutil.Uint64   `json:"block"`  // Block number corresponding to this trace
	Hash   common.Hash      `json:"hash"`   // Block hash corresponding to this trace
	Traces []*txTraceResult `json:"traces"` // Trace results produced by the task
}

// txTraceTask represents a single transaction trace task when an entire block
// is being traced.
type txTraceTask struct {
	statedb          *state.StateDB // Intermediate state prepped for tracing
	index            int            // Transaction offset in the block
	taskExtraContext map[string]interface{}
}

// TraceChain returns the structured logs created during the execution of EVM
// between two blocks (excluding start) and returns them as a JSON object.
func (api *PrivateDebugAPI) TraceChain(ctx context.Context, start, end rpc.BlockNumber, config *TraceConfig) (*rpc.Subscription, error) {
	// Fetch the block interval that we want to trace
	var from, to *types.Block

	switch start {
	case rpc.PendingBlockNumber:
		from = api.eth.miner.PendingBlock()
	case rpc.LatestBlockNumber:
		from = api.eth.blockchain.CurrentBlock()
	default:
		from = api.eth.blockchain.GetBlockByNumber(uint64(start))
	}
	switch end {
	case rpc.PendingBlockNumber:
		to = api.eth.miner.PendingBlock()
	case rpc.LatestBlockNumber:
		to = api.eth.blockchain.CurrentBlock()
	default:
		to = api.eth.blockchain.GetBlockByNumber(uint64(end))
	}
	// Trace the chain if we've found all our blocks
	if from == nil {
		return nil, fmt.Errorf("starting block #%d not found", start)
	}
	if to == nil {
		return nil, fmt.Errorf("end block #%d not found", end)
	}
	if from.Number().Cmp(to.Number()) >= 0 {
		return nil, fmt.Errorf("end block (#%d) needs to come after start block (#%d)", end, start)
	}
	return api.traceChain(ctx, from, to, config)
}

// traceChain configures a new tracer according to the provided configuration, and
// executes all the transactions contained within. The return value will be one item
// per transaction, dependent on the requested tracer.
func traceChain(ctx context.Context, eth *Ethereum, start, end *types.Block, config *TraceConfig) (*rpc.Subscription, error) {
	// Tracing a chain is a **long** operation, only do with subscriptions
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}
	sub := notifier.CreateSubscription()

	// Ensure we have a valid starting state before doing any work
	origin := start.NumberU64()
	database := state.NewDatabaseWithCache(eth.ChainDb(), 16, "") // Chain tracing will probably start at genesis

	if number := start.NumberU64(); number > 0 {
		start = eth.blockchain.GetBlock(start.ParentHash(), start.NumberU64()-1)
		if start == nil {
			return nil, fmt.Errorf("parent block #%d not found", number-1)
		}
	}
	statedb, err := state.New(start.Root(), database, nil)
	if err != nil {
		// If the starting state is missing, allow some number of blocks to be reexecuted
		reexec := defaultTraceReexec
		if config != nil && config.Reexec != nil {
			reexec = *config.Reexec
		}
		// Find the most recent block that has the state available
		for i := uint64(0); i < reexec; i++ {
			start = eth.blockchain.GetBlock(start.ParentHash(), start.NumberU64()-1)
			if start == nil {
				break
			}
			if statedb, err = state.New(start.Root(), database, nil); err == nil {
				break
			}
		}
		// If we still don't have the state available, bail out
		if err != nil {
			switch err.(type) {
			case *trie.MissingNodeError:
				return nil, errors.New("required historical state unavailable")
			default:
				return nil, err
			}
		}
	}
	// Execute all the transaction contained within the chain concurrently for each block
	blocks := int(end.NumberU64() - origin)

	threads := runtime.NumCPU()
	if threads > blocks {
		threads = blocks
	}
	var (
		pend    = new(sync.WaitGroup)
		tasks   = make(chan *blockTraceTask, threads)
		results = make(chan *blockTraceTask, threads)
	)
	for th := 0; th < threads; th++ {
		pend.Add(1)
		go func() {
			defer pend.Done()

			// Fetch and execute the next block trace tasks
			for task := range tasks {
				signer := types.MakeSigner(eth.blockchain.Config(), task.block.Number())

				// Trace all the transactions contained within
				for i, tx := range task.block.Transactions() {
					msg, _ := tx.AsMessage(signer)
					vmctx := core.NewEVMContext(msg, task.block.Header(), eth.blockchain, nil)

					res, err := traceTx(ctx, eth, msg, vmctx, task.statedb, nil, config)
					if err != nil {
						task.results[i] = &txTraceResult{Error: err.Error()}
						log.Warn("Tracing failed", "hash", tx.Hash(), "block", task.block.NumberU64(), "err", err)
						break
					}
					// Only delete empty objects if EIP158/161 (a.k.a Spurious Dragon) is in effect
					task.statedb.Finalise(eth.blockchain.Config().IsEnabled(eth.blockchain.Config().GetEIP161dTransition, task.block.Number()))
					task.results[i] = &txTraceResult{Result: res}
				}
				// Stream the result back to the user or abort on teardown
				select {
				case results <- task:
				case <-notifier.Closed():
					return
				}
			}
		}()
	}
	// Start a goroutine to feed all the blocks into the tracers
	begin := time.Now()

	go func() {
		var (
			logged time.Time
			number uint64
			traced uint64
			failed error
			proot  common.Hash
		)
		// Ensure everything is properly cleaned up on any exit path
		defer func() {
			close(tasks)
			pend.Wait()

			switch {
			case failed != nil:
				log.Warn("Chain tracing failed", "start", start.NumberU64(), "end", end.NumberU64(), "transactions", traced, "elapsed", time.Since(begin), "err", failed)
			case number < end.NumberU64():
				log.Warn("Chain tracing aborted", "start", start.NumberU64(), "end", end.NumberU64(), "abort", number, "transactions", traced, "elapsed", time.Since(begin))
			default:
				log.Info("Chain tracing finished", "start", start.NumberU64(), "end", end.NumberU64(), "transactions", traced, "elapsed", time.Since(begin))
			}
			close(results)
		}()
		// Feed all the blocks both into the tracer, as well as fast process concurrently
		for number = start.NumberU64() + 1; number <= end.NumberU64(); number++ {
			// Stop tracing if interruption was requested
			select {
			case <-notifier.Closed():
				return
			default:
			}
			// Print progress logs if long enough time elapsed
			if time.Since(logged) > 8*time.Second {
				if number > origin {
					nodes, imgs := database.TrieDB().Size()
					log.Info("Tracing chain segment", "start", origin, "end", end.NumberU64(), "current", number, "transactions", traced, "elapsed", time.Since(begin), "memory", nodes+imgs)
				} else {
					log.Info("Preparing state for chain trace", "block", number, "start", origin, "elapsed", time.Since(begin))
				}
				logged = time.Now()
			}
			// Retrieve the next block to trace
			block := eth.blockchain.GetBlockByNumber(number)
			if block == nil {
				failed = fmt.Errorf("block #%d not found", number)
				break
			}
			// Send the block over to the concurrent tracers (if not in the fast-forward phase)
			if number > origin {
				txs := block.Transactions()

				select {
				case tasks <- &blockTraceTask{statedb: statedb.Copy(), block: block, rootref: proot, results: make([]*txTraceResult, len(txs))}:
				case <-notifier.Closed():
					return
				}
				traced += uint64(len(txs))
			}
			// Generate the next state snapshot fast without tracing
			_, _, _, err := eth.blockchain.Processor().Process(block, statedb, vm.Config{})
			if err != nil {
				failed = err
				break
			}
			// Finalize the state so any modifications are written to the trie
			root, err := statedb.Commit(eth.blockchain.Config().IsEnabled(eth.blockchain.Config().GetEIP161dTransition, block.Number()))
			if err != nil {
				failed = err
				break
			}
			if err := statedb.Reset(root); err != nil {
				failed = err
				break
			}
			// Reference the trie twice, once for us, once for the tracer
			database.TrieDB().Reference(root, common.Hash{})
			if number >= origin {
				database.TrieDB().Reference(root, common.Hash{})
			}
			// Dereference all past tries we ourselves are done working with
			if proot != (common.Hash{}) {
				database.TrieDB().Dereference(proot)
			}
			proot = root

			// TODO(karalabe): Do we need the preimages? Won't they accumulate too much?
		}
	}()

	// Keep reading the trace results and stream them to the user
	go func() {
		var (
			done = make(map[uint64]*blockTraceResult)
			next = origin + 1
		)
		for res := range results {
			// Queue up next received result
			result := &blockTraceResult{
				Block:  hexutil.Uint64(res.block.NumberU64()),
				Hash:   res.block.Hash(),
				Traces: res.results,
			}
			done[uint64(result.Block)] = result

			// Dereference any paret tries held in memory by this task
			database.TrieDB().Dereference(res.rootref)

			// Stream completed traces to the user, aborting on the first error
			for result, ok := done[next]; ok; result, ok = done[next] {
				if len(result.Traces) > 0 || next == end.NumberU64() {
					notifier.Notify(sub.ID, result)
				}
				delete(done, next)
				next++
			}
		}
	}()
	return sub, nil
}

// traceChain configures a new tracer according to the provided configuration, and
// executes all the transactions contained within. The return value will be one item
// per transaction, dependent on the requested tracer.
func (api *PrivateDebugAPI) traceChain(ctx context.Context, start, end *types.Block, config *TraceConfig) (*rpc.Subscription, error) {
	return traceChain(ctx, api.eth, start, end, config)
}

// TraceBlockByNumber returns the structured logs created during the execution of
// EVM and returns them as a JSON object.
func traceBlockByNumber(ctx context.Context, eth *Ethereum, number rpc.BlockNumber, config *TraceConfig) ([]*txTraceResult, error) {
	// Fetch the block that we want to trace
	var block *types.Block

	switch number {
	case rpc.PendingBlockNumber:
		block = eth.miner.PendingBlock()
	case rpc.LatestBlockNumber:
		block = eth.blockchain.CurrentBlock()
	default:
		block = eth.blockchain.GetBlockByNumber(uint64(number))
	}
	// Trace the block if it was found
	if block == nil {
		return nil, fmt.Errorf("block #%d not found", number)
	}
	return traceBlock(ctx, eth, block, config)
}

// TraceBlockByNumber returns the structured logs created during the execution of
// EVM and returns them as a JSON object.
func (api *PrivateDebugAPI) TraceBlockByNumber(ctx context.Context, number rpc.BlockNumber, config *TraceConfig) ([]*txTraceResult, error) {
	return traceBlockByNumber(ctx, api.eth, number, config)
}

// TraceBlockByHash returns the structured logs created during the execution of
// EVM and returns them as a JSON object.
func (api *PrivateDebugAPI) TraceBlockByHash(ctx context.Context, hash common.Hash, config *TraceConfig) ([]*txTraceResult, error) {
	block := api.eth.blockchain.GetBlockByHash(hash)
	if block == nil {
		return nil, fmt.Errorf("block %#x not found", hash)
	}
	return api.traceBlock(ctx, block, config)
}

// traceBlockRLP returns the structured logs created during the execution of EVM
// and returns them as a JSON object.
func traceBlockRLP(ctx context.Context, eth *Ethereum, blob []byte, config *TraceConfig) ([]*txTraceResult, error) {
	block := new(types.Block)
	if err := rlp.Decode(bytes.NewReader(blob), block); err != nil {
		return nil, fmt.Errorf("could not decode block: %v", err)
	}
	return traceBlock(ctx, eth, block, config)
}

// TraceBlock returns the structured logs created during the execution of EVM
// and returns them as a JSON object.
func (api *PrivateDebugAPI) TraceBlock(ctx context.Context, blob []byte, config *TraceConfig) ([]*txTraceResult, error) {
	return traceBlockRLP(ctx, api.eth, blob, config)
}

// TraceBlockFromFile returns the structured logs created during the execution of
// EVM and returns them as a JSON object.
func (api *PrivateDebugAPI) TraceBlockFromFile(ctx context.Context, file string, config *TraceConfig) ([]*txTraceResult, error) {
	blob, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("could not read file: %v", err)
	}
	return api.TraceBlock(ctx, blob, config)
}

// TraceBadBlock returns the structured logs created during the execution of
// EVM against a block pulled from the pool of bad ones and returns them as a JSON
// object.
func (api *PrivateDebugAPI) TraceBadBlock(ctx context.Context, hash common.Hash, config *TraceConfig) ([]*txTraceResult, error) {
	blocks := api.eth.blockchain.BadBlocks()
	for _, block := range blocks {
		if block.Hash() == hash {
			return api.traceBlock(ctx, block, config)
		}
	}
	return nil, fmt.Errorf("bad block %#x not found", hash)
}

// StandardTraceBlockToFile dumps the structured logs created during the
// execution of EVM to the local file system and returns a list of files
// to the caller.
func (api *PrivateDebugAPI) StandardTraceBlockToFile(ctx context.Context, hash common.Hash, config *StdTraceConfig) ([]string, error) {
	block := api.eth.blockchain.GetBlockByHash(hash)
	if block == nil {
		return nil, fmt.Errorf("block %#x not found", hash)
	}
	return api.standardTraceBlockToFile(ctx, block, config)
}

// StandardTraceBadBlockToFile dumps the structured logs created during the
// execution of EVM against a block pulled from the pool of bad ones to the
// local file system and returns a list of files to the caller.
func (api *PrivateDebugAPI) StandardTraceBadBlockToFile(ctx context.Context, hash common.Hash, config *StdTraceConfig) ([]string, error) {
	blocks := api.eth.blockchain.BadBlocks()
	for _, block := range blocks {
		if block.Hash() == hash {
			return api.standardTraceBlockToFile(ctx, block, config)
		}
	}
	return nil, fmt.Errorf("bad block %#x not found", hash)
}

// traceBlock configures a new tracer according to the provided configuration, and
// executes all the transactions contained within. The return value will be one item
// per transaction, dependent on the requestd tracer.
func traceBlock(ctx context.Context, eth *Ethereum, block *types.Block, config *TraceConfig) ([]*txTraceResult, error) {
	// Create the parent state database
	if err := eth.engine.VerifyHeader(eth.blockchain, block.Header(), true); err != nil {
		return nil, err
	}
	parent := eth.blockchain.GetBlock(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return nil, fmt.Errorf("parent %#x not found", block.ParentHash())
	}
	reexec := defaultTraceReexec
	if config != nil && config.Reexec != nil {
		reexec = *config.Reexec
	}
	statedb, err := computeStateDB(eth, parent, reexec)
	if err != nil {
		return nil, err
	}
	// Execute all the transaction contained within the block concurrently
	var (
		signer = types.MakeSigner(eth.blockchain.Config(), block.Number())

		txs     = block.Transactions()
		results = make([]*txTraceResult, len(txs))

		pend = new(sync.WaitGroup)
		jobs = make(chan *txTraceTask, len(txs))
	)
	threads := runtime.NumCPU()
	if threads > len(txs) {
		threads = len(txs)
	}
	for th := 0; th < threads; th++ {
		pend.Add(1)
		go func() {
			defer pend.Done()

			// Fetch and execute the next transaction trace tasks
			for task := range jobs {
				msg, _ := txs[task.index].AsMessage(signer)
				vmctx := core.NewEVMContext(msg, block.Header(), eth.blockchain, nil)

				res, err := traceTx(ctx, eth, msg, vmctx, task.statedb, task.taskExtraContext, config)
				if err != nil {
					results[task.index] = &txTraceResult{Error: err.Error()}
					continue
				}
				results[task.index] = &txTraceResult{Result: res}
			}
		}()
	}
	// Feed the transactions into the tracers and return
	var failed error
	for i, tx := range txs {
		taskExtraContext := map[string]interface{}{
			"blockNumber":         block.NumberU64(),
			"blockHash":           block.Hash().Hex(),
			"transactionHash":     tx.Hash().Hex(),
			"transactionPosition": uint64(i),
		}

		// Send the trace task over for execution
		jobs <- &txTraceTask{statedb: statedb.Copy(), index: i, taskExtraContext: taskExtraContext}

		// Generate the next state snapshot fast without tracing
		msg, _ := tx.AsMessage(signer)
		vmctx := core.NewEVMContext(msg, block.Header(), eth.blockchain, nil)

		vmenv := vm.NewEVM(vmctx, statedb, eth.blockchain.Config(), vm.Config{})
		if _, err := core.ApplyMessage(vmenv, msg, new(core.GasPool).AddGas(msg.Gas())); err != nil {
			failed = err
			break
		}
		// Finalize the state so any modifications are written to the trie
		// Only delete empty objects if EIP158/161 (a.k.a Spurious Dragon) is in effect
		statedb.Finalise(vmenv.ChainConfig().IsEnabled(vmenv.ChainConfig().GetEIP161dTransition, block.Number()))
	}
	close(jobs)
	pend.Wait()

	// If execution failed in between, abort
	if failed != nil {
		return nil, failed
	}
	return results, nil
}

func (api *PrivateDebugAPI) traceBlock(ctx context.Context, block *types.Block, config *TraceConfig) ([]*txTraceResult, error) {
	return traceBlock(ctx, api.eth, block, config)
}

// standardTraceBlockToFile configures a new tracer which uses standard JSON output,
// and traces either a full block or an individual transaction. The return value will
// be one filename per transaction traced.
func (api *PrivateDebugAPI) standardTraceBlockToFile(ctx context.Context, block *types.Block, config *StdTraceConfig) ([]string, error) {
	// If we're tracing a single transaction, make sure it's present
	if config != nil && config.TxHash != (common.Hash{}) {
		if !containsTx(block, config.TxHash) {
			return nil, fmt.Errorf("transaction %#x not found in block", config.TxHash)
		}
	}
	// Create the parent state database
	if err := api.eth.engine.VerifyHeader(api.eth.blockchain, block.Header(), true); err != nil {
		return nil, err
	}
	parent := api.eth.blockchain.GetBlock(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return nil, fmt.Errorf("parent %#x not found", block.ParentHash())
	}
	reexec := defaultTraceReexec
	if config != nil && config.Reexec != nil {
		reexec = *config.Reexec
	}
	statedb, err := computeStateDB(api.eth, parent, reexec)
	if err != nil {
		return nil, err
	}
	// Retrieve the tracing configurations, or use default values
	var (
		logConfig vm.LogConfig
		txHash    common.Hash
	)
	if config != nil {
		if config.LogConfig != nil {
			logConfig = *config.LogConfig
		}
		txHash = config.TxHash
	}
	logConfig.Debug = true

	// Execute transaction, either tracing all or just the requested one
	var (
		signer      = types.MakeSigner(api.eth.blockchain.Config(), block.Number())
		dumps       []string
		chainConfig = api.eth.blockchain.Config()
		canon       = true
	)
	// Check if there are any overrides: the caller may wish to enable a future
	// fork when executing this block. Note, such overrides are only applicable to the
	// actual specified block, not any preceding blocks that we have to go through
	// in order to obtain the state.
	// Therefore, it's perfectly valid to specify `"futureForkBlock": 0`, to enable `futureFork`

	if config != nil && config.Overrides != nil {

		overrideEIP2929 := config.Overrides.GetEIP2929Transition()
		existingEIP2929 := chainConfig.GetEIP2929Transition()

		// We only need to make a copy if this transition is actually going to be overridden.
		// This very ugly logic assumes that testing value equivalence 4 times is cheaper than copying an arbitrary
		// (and probably biggish) interface value unnecessarily.
		if (overrideEIP2929 == nil && existingEIP2929 != nil) ||
			(overrideEIP2929 != nil && existingEIP2929 == nil) ||
			(overrideEIP2929 != nil && existingEIP2929 != nil && *overrideEIP2929 != *existingEIP2929) {

			// Copy the config, to not screw up the main config
			// Note: the Clique-part is _not_ deep copied

			// We know that the underlying configurator value will always be a pointer.
			chainConfig = reflect.New(reflect.ValueOf(chainConfig).Elem().Type()).Interface().(ctypes.ChainConfigurator)

			if err := chainConfig.SetEIP2929Transition(overrideEIP2929); err != nil {
				return nil, err
			}
			canon = false
		}
	}
	for i, tx := range block.Transactions() {
		// Prepare the trasaction for un-traced execution
		var (
			msg, _ = tx.AsMessage(signer)
			vmctx  = core.NewEVMContext(msg, block.Header(), api.eth.blockchain, nil)

			vmConf vm.Config
			dump   *os.File
			writer *bufio.Writer
			err    error
		)
		// If the transaction needs tracing, swap out the configs
		if tx.Hash() == txHash || txHash == (common.Hash{}) {
			// Generate a unique temporary file to dump it into
			prefix := fmt.Sprintf("block_%#x-%d-%#x-", block.Hash().Bytes()[:4], i, tx.Hash().Bytes()[:4])
			if !canon {
				prefix = fmt.Sprintf("%valt-", prefix)
			}
			dump, err = ioutil.TempFile(os.TempDir(), prefix)
			if err != nil {
				return nil, err
			}
			dumps = append(dumps, dump.Name())

			// Swap out the noop logger to the standard tracer
			writer = bufio.NewWriter(dump)
			vmConf = vm.Config{
				Debug:                   true,
				Tracer:                  vm.NewJSONLogger(&logConfig, writer),
				EnablePreimageRecording: true,
			}
		}
		// Execute the transaction and flush any traces to disk
		vmenv := vm.NewEVM(vmctx, statedb, chainConfig, vmConf)
		_, err = core.ApplyMessage(vmenv, msg, new(core.GasPool).AddGas(msg.Gas()))
		if writer != nil {
			writer.Flush()
		}
		if dump != nil {
			dump.Close()
			log.Info("Wrote standard trace", "file", dump.Name())
		}
		if err != nil {
			return dumps, err
		}
		// Finalize the state so any modifications are written to the trie
		// Only delete empty objects if EIP158/161 (a.k.a Spurious Dragon) is in effect
		statedb.Finalise(vmenv.ChainConfig().IsEnabled(vmenv.ChainConfig().GetEIP161dTransition, block.Number()))

		// If we've traced the transaction we were looking for, abort
		if tx.Hash() == txHash {
			break
		}
	}
	return dumps, nil
}

// containsTx reports whether the transaction with a certain hash
// is contained within the specified block.
func containsTx(block *types.Block, hash common.Hash) bool {
	for _, tx := range block.Transactions() {
		if tx.Hash() == hash {
			return true
		}
	}
	return false
}

// computeStateDB retrieves the state database associated with a certain block.
// If no state is locally available for the given block, a number of blocks are
// attempted to be reexecuted to generate the desired state.
func computeStateDB(eth *Ethereum, block *types.Block, reexec uint64) (*state.StateDB, error) {
	// If we have the state fully available, use that
	statedb, err := eth.blockchain.StateAt(block.Root())
	if err == nil {
		return statedb, nil
	}
	// Otherwise try to reexec blocks until we find a state or reach our limit
	origin := block.NumberU64()
	database := state.NewDatabaseWithCache(eth.ChainDb(), 16, "")

	for i := uint64(0); i < reexec; i++ {
		block = eth.blockchain.GetBlock(block.ParentHash(), block.NumberU64()-1)
		if block == nil {
			break
		}
		if statedb, err = state.New(block.Root(), database, nil); err == nil {
			break
		}
	}
	if err != nil {
		switch err.(type) {
		case *trie.MissingNodeError:
			return nil, fmt.Errorf("required historical state unavailable (reexec=%d)", reexec)
		default:
			return nil, err
		}
	}
	// State was available at historical point, regenerate
	var (
		start  = time.Now()
		logged time.Time
		proot  common.Hash
	)
	for block.NumberU64() < origin {
		// Print progress logs if long enough time elapsed
		if time.Since(logged) > 8*time.Second {
			log.Info("Regenerating historical state", "block", block.NumberU64()+1, "target", origin, "remaining", origin-block.NumberU64()-1, "elapsed", time.Since(start))
			logged = time.Now()
		}
		// Retrieve the next block to regenerate and process it
		if block = eth.blockchain.GetBlockByNumber(block.NumberU64() + 1); block == nil {
			return nil, fmt.Errorf("block #%d not found", block.NumberU64()+1)
		}
		_, _, _, err := eth.blockchain.Processor().Process(block, statedb, vm.Config{})
		if err != nil {
			return nil, fmt.Errorf("processing block %d failed: %v", block.NumberU64(), err)
		}
		// Finalize the state so any modifications are written to the trie
		root, err := statedb.Commit(eth.blockchain.Config().IsEnabled(eth.blockchain.Config().GetEIP161dTransition, block.Number()))
		if err != nil {
			return nil, err
		}
		if err := statedb.Reset(root); err != nil {
			return nil, fmt.Errorf("state reset after block %d failed: %v", block.NumberU64(), err)
		}
		database.TrieDB().Reference(root, common.Hash{})
		if proot != (common.Hash{}) {
			database.TrieDB().Dereference(proot)
		}
		proot = root
	}
	nodes, imgs := database.TrieDB().Size()
	log.Info("Historical state regenerated", "block", block.NumberU64(), "elapsed", time.Since(start), "nodes", nodes, "preimages", imgs)
	return statedb, nil
}

func (api *PrivateDebugAPI) computeStateDB(block *types.Block, reexec uint64) (*state.StateDB, error) {
	return computeStateDB(api.eth, block, reexec)
}

// traceTransaction returns the structured logs created during the execution of EVM
// and returns them as a JSON object.
func traceTransaction(ctx context.Context, eth *Ethereum, hash common.Hash, config *TraceConfig) (interface{}, error) {
	// Retrieve the transaction and assemble its EVM context
	tx, blockHash, _, index := rawdb.ReadTransaction(eth.ChainDb(), hash)
	if tx == nil {
		return nil, fmt.Errorf("transaction %#x not found", hash)
	}
	reexec := defaultTraceReexec
	if config != nil && config.Reexec != nil {
		reexec = *config.Reexec
	}
	// Retrieve the block
	block := eth.blockchain.GetBlockByHash(blockHash)
	if block == nil {
		return nil, fmt.Errorf("block %#x not found", blockHash)
	}
	msg, vmctx, statedb, err := computeTxEnv(eth, block, int(index), reexec)
	if err != nil {
		return nil, err
	}

	taskExtraContext := map[string]interface{}{
		"blockNumber":         block.NumberU64(),
		"blockHash":           blockHash.Hex(),
		"transactionHash":     tx.Hash().Hex(),
		"transactionPosition": index,
	}

	// Trace the transaction and return
	return traceTx(ctx, eth, msg, vmctx, statedb, taskExtraContext, config)
}

// TraceTransaction returns the structured logs created during the execution of EVM
// and returns them as a JSON object.
func (api *PrivateDebugAPI) TraceTransaction(ctx context.Context, hash common.Hash, config *TraceConfig) (interface{}, error) {
	return traceTransaction(ctx, api.eth, hash, config)
}

// traceCall lets you trace a given eth_call. It collects the structured logs created during the execution of EVM
// if the given transaction was added on top of the provided block and returns them as a JSON object.
// You can provide -2 as a block number to trace on top of the pending block.
func traceCall(ctx context.Context, eth *Ethereum, args ethapi.CallArgs, blockNrOrHash rpc.BlockNumberOrHash, config *TraceConfig) (interface{}, error) {
	// First try to retrieve the state
	blockNrOrHash.RequireCanonical = true
	statedb, header, err := eth.APIBackend.StateAndHeaderByNumberOrHash(ctx, blockNrOrHash)
	if err != nil {
		// Try to retrieve the specified block
		var block *types.Block
		if hash, ok := blockNrOrHash.Hash(); ok {
			block = eth.blockchain.GetBlockByHash(hash)
		} else if number, ok := blockNrOrHash.Number(); ok {
			block = eth.blockchain.GetBlockByNumber(uint64(number))
		}
		if block == nil {
			return nil, fmt.Errorf("block %v not found: %v", blockNrOrHash, err)
		}
		// try to recompute the state
		reexec := defaultTraceReexec
		if config != nil && config.Reexec != nil {
			reexec = *config.Reexec
		}
		_, _, statedb, err = computeTxEnv(eth, block, len(block.Transactions())-1, reexec)
		if err != nil {
			return nil, err
		}
	}

	// Execute the trace
	msg := args.ToMessage(eth.APIBackend.RPCGasCap())
	vmctx := core.NewEVMContext(msg, header, eth.blockchain, nil)

	originalCanTransfer := vmctx.CanTransfer
	originalTransfer := vmctx.Transfer

	// Store the truth on wether from acount has enough balance for context usage
	gasCost := new(big.Int).Mul(new(big.Int).SetUint64(msg.Gas()), msg.GasPrice())
	totalCost := new(big.Int).Add(gasCost, msg.Value())
	hasFromSufficientBalanceForValueAndGasCost := vmctx.CanTransfer(statedb, msg.From(), totalCost)
	hasFromSufficientBalanceForGasCost := vmctx.CanTransfer(statedb, msg.From(), gasCost)

	// This is needed for trace_call (debug mode),
	// as the Transaction is being run on top of the block transactions,
	// which might lead into ErrInsufficientFundsForTransfer error
	vmctx.CanTransfer = func(db vm.StateDB, sender common.Address, amount *big.Int) bool {
		if msg.From() == sender {
			return true
		}
		res := originalCanTransfer(db, sender, amount)
		return res
	}

	// If the actual transaction would fail, then their is no reason to actually transfer any balance at all
	vmctx.Transfer = func(db vm.StateDB, sender, recipient common.Address, amount *big.Int) {
		toAmount := new(big.Int).Set(amount)

		senderBalance := db.GetBalance(sender)
		if senderBalance.Cmp(toAmount) < 0 {
			toAmount.Set(big.NewInt(0))
		}

		originalTransfer(db, sender, recipient, toAmount)
	}

	// Add extra context needed for state_diff
	taskExtraContext := map[string]interface{}{
		"hasFromSufficientBalanceForValueAndGasCost": hasFromSufficientBalanceForValueAndGasCost,
		"hasFromSufficientBalanceForGasCost":         hasFromSufficientBalanceForGasCost,
	}

	return traceTx(ctx, eth, msg, vmctx, statedb, taskExtraContext, config)
}

// TraceCall lets you trace a given eth_call. It collects the structured logs created during the execution of EVM
// if the given transaction was added on top of the provided block and returns them as a JSON object.
// You can provide -2 as a block number to trace on top of the pending block.
func (api *PrivateDebugAPI) TraceCall(ctx context.Context, args ethapi.CallArgs, blockNrOrHash rpc.BlockNumberOrHash, config *TraceConfig) (interface{}, error) {
	return traceCall(ctx, api.eth, args, blockNrOrHash, config)
}

// TraceCallMany lets you trace a given eth_call. It collects the structured logs created during the execution of EVM
// if the given transaction was added on top of the provided block and returns them as a JSON object.
// You can provide -2 as a block number to trace on top of the pending block.
func traceCallMany(ctx context.Context, eth *Ethereum, txs []ethapi.CallArgs, blockNrOrHash rpc.BlockNumberOrHash, config *TraceConfig) (interface{}, error) {
	// First try to retrieve the state
	statedb, header, err := eth.APIBackend.StateAndHeaderByNumberOrHash(ctx, blockNrOrHash)
	if err != nil {
		// Try to retrieve the specified block
		var block *types.Block
		if hash, ok := blockNrOrHash.Hash(); ok {
			block = eth.blockchain.GetBlockByHash(hash)
		} else if number, ok := blockNrOrHash.Number(); ok {
			block = eth.blockchain.GetBlockByNumber(uint64(number))
		}
		if block == nil {
			return nil, fmt.Errorf("block %v not found: %v", blockNrOrHash, err)
		}
		// try to recompute the state
		reexec := defaultTraceReexec
		if config != nil && config.Reexec != nil {
			reexec = *config.Reexec
		}
		_, _, statedb, err = computeTxEnv(eth, block, len(block.Transactions())-1, reexec)
		if err != nil {
			return nil, err
		}
	}

	// Execute the trace
	var results = make([]interface{}, len(txs))
	for idx, args := range txs {
		msg := args.ToMessage(eth.APIBackend.RPCGasCap())
		vmctx := core.NewEVMContext(msg, header, eth.blockchain, nil)

		originalCanTransfer := vmctx.CanTransfer
		originalTransfer := vmctx.Transfer

		// Store the truth on wether from acount has enough balance for context usage
		gasCost := new(big.Int).Mul(new(big.Int).SetUint64(msg.Gas()), msg.GasPrice())
		totalCost := new(big.Int).Add(gasCost, msg.Value())
		hasFromSufficientBalanceForValueAndGasCost := vmctx.CanTransfer(statedb, msg.From(), totalCost)
		hasFromSufficientBalanceForGasCost := vmctx.CanTransfer(statedb, msg.From(), gasCost)

		// This is needed for trace_call (debug mode),
		// as the Transaction is being run on top of the block transactions,
		// which might lead into ErrInsufficientFundsForTransfer error
		vmctx.CanTransfer = func(db vm.StateDB, sender common.Address, amount *big.Int) bool {
			if msg.From() == sender {
				return true
			}
			res := originalCanTransfer(db, sender, amount)
			return res
		}

		// If the actual transaction would fail, then their is no reason to actually transfer any balance at all
		vmctx.Transfer = func(db vm.StateDB, sender, recipient common.Address, amount *big.Int) {
			toAmount := new(big.Int).Set(amount)

			senderBalance := db.GetBalance(sender)
			if senderBalance.Cmp(toAmount) < 0 {
				toAmount.Set(big.NewInt(0))
			}

			originalTransfer(db, sender, recipient, toAmount)
		}

		// Add extra context needed for state_diff
		taskExtraContext := map[string]interface{}{
			"hasFromSufficientBalanceForValueAndGasCost": hasFromSufficientBalanceForValueAndGasCost,
			"hasFromSufficientBalanceForGasCost":         hasFromSufficientBalanceForGasCost,
		}

		res, err := traceTx(ctx, eth, msg, vmctx, statedb, taskExtraContext, config)
		if err != nil {
			results[idx] = &txTraceResult{Error: err.Error()}
			continue
		}

		res, err = decorateResponse(res, config)
		if err != nil {
			return nil, fmt.Errorf("failed to decorate response for transaction at index %d with error %v", idx, err)
		}
		results[idx] = res
	}

	return results, nil
}

// traceTx configures a new tracer according to the provided configuration, and
// executes the given message in the provided environment. The return value will
// be tracer dependent.
func traceTx(ctx context.Context, eth *Ethereum, message core.Message, vmctx vm.Context, statedb *state.StateDB, extraContext map[string]interface{}, config *TraceConfig) (interface{}, error) {
	// Assemble the structured logger or the JavaScript tracer
	var (
		tracer vm.Tracer
		err    error
	)
	switch {
	case config != nil && config.Tracer != nil:
		// Define a meaningful timeout of a single transaction trace
		timeout := defaultTraceTimeout
		if config.Timeout != nil {
			if timeout, err = time.ParseDuration(*config.Timeout); err != nil {
				return nil, err
			}
		}
		// Constuct the JavaScript tracer to execute with
		if tracer, err = tracers.New(*config.Tracer); err != nil {
			return nil, err
		}
		// Handle timeouts and RPC cancellations
		deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
		go func() {
			<-deadlineCtx.Done()
			tracer.(*tracers.Tracer).Stop(errors.New("execution timeout"))
		}()
		defer cancel()

	case config == nil:
		tracer = vm.NewStructLogger(nil)

	default:
		tracer = vm.NewStructLogger(config.LogConfig)
	}
	// Run the transaction with tracing enabled.
	vmenv := vm.NewEVM(vmctx, statedb, eth.blockchain.Config(), vm.Config{Debug: true, Tracer: tracer})

	switch tracer := tracer.(type) {
	case *tracers.Tracer:
		if extraContext == nil {
			extraContext = map[string]interface{}{}
		}

		// Add useful context for all tracers
		extraContext["from"] = message.From()
		if message.To() != nil {
			extraContext["msgTo"] = *message.To()
		}
		extraContext["coinbase"] = vmctx.Coinbase

		extraContext["gasLimit"] = message.Gas()
		extraContext["gasPrice"] = message.GasPrice()

		tracer.CapturePreEVM(vmenv, extraContext)
	}

	result, err := core.ApplyMessage(vmenv, message, new(core.GasPool).AddGas(message.Gas()))
	if err != nil {
		return nil, fmt.Errorf("tracing failed: %v", err)
	}
	// Depending on the tracer type, format and return the output
	switch tracer := tracer.(type) {
	case *vm.StructLogger:
		// If the result contains a revert reason, return it.
		returnVal := fmt.Sprintf("%x", result.Return())
		if len(result.Revert()) > 0 {
			returnVal = fmt.Sprintf("%x", result.Revert())
		}
		return &ethapi.ExecutionResult{
			Gas:         result.UsedGas,
			Failed:      result.Failed(),
			ReturnValue: returnVal,
			StructLogs:  ethapi.FormatLogs(tracer.StructLogs()),
		}, nil

	case *tracers.Tracer:
		return tracer.GetResult()

	default:
		panic(fmt.Sprintf("bad tracer type %T", tracer))
	}
}

// traceTx configures a new tracer according to the provided configuration, and
// executes the given message in the provided environment. The return value will
// be tracer dependent.
func (api *PrivateDebugAPI) traceTx(ctx context.Context, message core.Message, vmctx vm.Context, statedb *state.StateDB, config *TraceConfig) (interface{}, error) {
	return traceTx(ctx, api.eth, message, vmctx, statedb, nil, config)
}

// computeTxEnv returns the execution environment of a certain transaction.
func computeTxEnv(eth *Ethereum, block *types.Block, txIndex int, reexec uint64) (core.Message, vm.Context, *state.StateDB, error) {
	// Create the parent state database
	parent := eth.blockchain.GetBlock(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return nil, vm.Context{}, nil, fmt.Errorf("parent %#x not found", block.ParentHash())
	}
	statedb, err := computeStateDB(eth, parent, reexec)
	if err != nil {
		return nil, vm.Context{}, nil, err
	}

	if txIndex == 0 && len(block.Transactions()) == 0 {
		return nil, vm.Context{}, statedb, nil
	}

	// Recompute transactions up to the target index.
	signer := types.MakeSigner(eth.blockchain.Config(), block.Number())

	for idx, tx := range block.Transactions() {
		// Assemble the transaction call message and return if the requested offset
		msg, _ := tx.AsMessage(signer)
		context := core.NewEVMContext(msg, block.Header(), eth.blockchain, nil)
		if idx == txIndex {
			return msg, context, statedb, nil
		}
		// Not yet the searched for transaction, execute on top of the current state
		vmenv := vm.NewEVM(context, statedb, eth.blockchain.Config(), vm.Config{})
		if _, err := core.ApplyMessage(vmenv, msg, new(core.GasPool).AddGas(tx.Gas())); err != nil {
			return nil, vm.Context{}, nil, fmt.Errorf("transaction %#x failed: %v", tx.Hash(), err)
		}
		// Ensure any modifications are committed to the state
		// Only delete empty objects if EIP158/161 (a.k.a Spurious Dragon) is in effect
		statedb.Finalise(vmenv.ChainConfig().IsEnabled(vmenv.ChainConfig().GetEIP161dTransition, block.Number()))
	}
	return nil, vm.Context{}, nil, fmt.Errorf("transaction index %d out of range for block %#x", txIndex, block.Hash())
}

func (api *PrivateDebugAPI) computeTxEnv(block *types.Block, txIndex int, reexec uint64) (core.Message, vm.Context, *state.StateDB, error) {
	return computeTxEnv(api.eth, block, txIndex, reexec)
}
