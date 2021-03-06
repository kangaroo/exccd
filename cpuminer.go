// Copyright (c) 2018 The ExchangeCoin team
// Copyright (c) 2014-2016 The btcsuite developers
// Copyright (c) 2015-2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/EXCCoin/exccd/blockchain"
	equihash "github.com/EXCCoin/exccd/cequihash"
	"github.com/EXCCoin/exccd/chaincfg"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/exccutil"
	"github.com/EXCCoin/exccd/mining"
	"github.com/EXCCoin/exccd/wire"
	"sync"
	"time"
	"unsafe"
)

const (
	// maxNonce is the maximum value a nonce can be in a block header.
	maxNonce = ^uint32(0) // 2^32 - 1

	// maxExtraNonce is the maximum value an extra nonce used in a coinbase
	// transaction can be.
	maxExtraNonce = ^uint64(0) // 2^64 - 1

	// hpsUpdateSecs is the number of seconds to wait in between each
	// update to the hashes per second monitor.
	hpsUpdateSecs = 60

	// hashUpdateSec is the number of seconds each worker waits in between
	// notifying the speed monitor with how many hashes have been completed
	// while they are actively searching for a solution.  This is done to
	// reduce the amount of syncs between the workers that must be done to
	// keep track of the hashes per second.
	hashUpdateSecs = 15

	// maxSimnetToMine is the maximum number of blocks to mine on HEAD~1
	// for simnet so that you don't run out of memory if tickets for
	// some reason run out during simulations.
	maxSimnetToMine uint8 = 4
)

var (
	// defaultNumWorkers is the default number of workers to use for mining
	// and is based on the number of processor cores.  This helps ensure the
	// system stays reasonably responsive under heavy load.
	defaultNumWorkers = uint32(chaincfg.CPUMinerThreads)

	// littleEndian is a convenience variable since binary.LittleEndian is
	// quite long.
	littleEndian = binary.LittleEndian
)

// CPUMiner provides facilities for solving blocks (mining) using the CPU in
// a concurrency-safe manner.  It consists of two main goroutines -- a speed
// monitor and a controller for worker goroutines which generate and solve
// blocks.  The number of goroutines can be set via the SetMaxGoRoutines
// function, but the default is based on the number of processor cores in the
// system which is typically sufficient.
type CPUMiner struct {
	sync.Mutex
	policy            *mining.Policy
	txSource          mining.TxSource
	server            *server
	numWorkers        uint32
	started           bool
	discreteMining    bool
	miningAddr        *exccutil.Address
	submitBlockLock   sync.Mutex
	wg                sync.WaitGroup
	workerWg          sync.WaitGroup
	updateNumWorkers  chan struct{}
	queryHashesPerSec chan float64
	updateHashes      chan uint64
	speedMonitorQuit  chan struct{}
	quit              chan struct{}

	// This is a map that keeps track of how many blocks have
	// been mined on each parent by the CPUMiner. It is only
	// for use in simulation networks, to diminish memory
	// exhaustion. It should not race because it's only
	// accessed in a single threaded loop below.
	minedOnParents map[chainhash.Hash]uint8
}

// speedMonitor handles tracking the number of hashes per second the mining
// process is performing.  It must be run as a goroutine.
func (m *CPUMiner) speedMonitor() {
	minrLog.Tracef("CPU miner speed monitor started")

	var updateCount uint64
	var totalHashes uint64
	var hashesCompletedInLastHour list.List
	var hashesPerSec float64
	ticker := time.NewTicker(time.Second * hpsUpdateSecs)
	defer ticker.Stop()

out:
	for {
		select {
		// Periodic updates from the workers with how many hashes they have performed.
		case numHashes := <-m.updateHashes:
			totalHashes += numHashes
			for i := uint64(0); i < numHashes; i++ {
				hashesCompletedInLastHour.PushBack(time.Now().Unix())
			}

		case <-ticker.C: // Time to update the hashes per second.
			var toRemove []*list.Element
			now := time.Now()
			for e := hashesCompletedInLastHour.Front(); e != nil; e = e.Next() {
				if now.Sub(time.Unix(e.Value.(int64), 0)).Hours() > 1 {
					toRemove = append(toRemove, e)
				} else {
					break
				}
			}

			for _, e := range toRemove {
				hashesCompletedInLastHour.Remove(e)
			}

			updateCount += 1
			startedSecsAgo := hpsUpdateSecs * updateCount
			if float64(startedSecsAgo) < time.Hour.Seconds() {
				hashesPerSec = float64(hashesCompletedInLastHour.Len()) / float64(startedSecsAgo)
			} else {
				hashesPerSec = float64(hashesCompletedInLastHour.Len()) / time.Hour.Seconds()
			}
			hashesPerHour := hashesPerSec * time.Hour.Seconds()
			if hashesPerHour != 0 {
				minrLog.Infof("Hash speed: %.2f hashes/hour, hashes completed: %d", hashesPerHour, totalHashes)
			}

		case m.queryHashesPerSec <- hashesPerSec: // Request for the number of hashes per second.
			// Nothing to do.

		case <-m.speedMonitorQuit:
			break out
		}
	}

	m.wg.Done()
	minrLog.Tracef("CPU miner speed monitor done")
}

// submitBlock submits the passed block to network after ensuring it passes all
// of the consensus validation rules.
func (m *CPUMiner) submitBlock(block *exccutil.Block) bool {
	m.submitBlockLock.Lock()
	defer m.submitBlockLock.Unlock()

	// Process this block using the same rules as blocks coming from other
	// nodes. This will in turn relay it to the network like normal.
	isOrphan, err := m.server.blockManager.ProcessBlock(block, blockchain.BFNone)
	if err != nil {
		// Anything other than a rule violation is an unexpected error,
		// so log that error as an internal error.
		rErr, ok := err.(blockchain.RuleError)
		if !ok {
			minrLog.Errorf("Unexpected error while processing block submitted via CPU miner: %v", err)
			return false
		}
		// Occasionally errors are given out for timing errors with
		// ReduceMinDifficulty and high block works that is above
		// the target. Feed these to debug.
		if m.server.chainParams.ReduceMinDifficulty &&
			rErr.ErrorCode == blockchain.ErrHighHash {
			minrLog.Debugf("Block submitted via CPU miner rejected because of ReduceMinDifficulty time sync "+
				"failure: %v", err)
			return false
		}
		// Other rule errors should be reported.
		minrLog.Errorf("Block submitted via CPU miner rejected: %v", err)
		return false

	}
	if isOrphan {
		minrLog.Errorf("Block submitted via CPU miner is an orphan building on parent %v",
			block.MsgBlock().Header.PrevBlock)
		return false
	}

	// The block was accepted.
	coinbaseTxOuts := block.MsgBlock().Transactions[0].TxOut
	coinbaseTxGenerated := int64(0)
	for _, out := range coinbaseTxOuts {
		coinbaseTxGenerated += out.Value
	}
	minrLog.Infof("Block submitted via CPU miner accepted (hash %s, height %v, amount %v)",
		block.Hash(), block.Height(), exccutil.Amount(coinbaseTxGenerated))
	return true
}

type solutionValidatorData struct {
	solved   *bool
	exiting  *bool
	msgBlock *wire.MsgBlock
	miner    *CPUMiner
	quit     chan struct{}
}

// returns 1 when mining should be stopped for any reason
func (data solutionValidatorData) Validate(solution unsafe.Pointer) int {
	bestBlock, _ := data.miner.server.blockManager.chainState.Best()
	if data.msgBlock.Header.PrevBlock != *bestBlock {
		*data.exiting = true
		return 1
	}

	if uintptr(solution) == 0 {
		if *data.exiting {
			minrLog.Infof("Shutdown is pending. Bailing out")
			return 1
		}
		select {
		case <-data.quit:
			minrLog.Infof("Miner is stopping")
			*data.exiting = true
			return 1
		default:
		}

		return 0
	}

	data.miner.updateHashes <- 1

	bytes := equihash.ExtractSolution(data.miner.server.chainParams.N, data.miner.server.chainParams.K, solution)
	copy(data.msgBlock.Header.EquihashSolution[:], bytes)
	hash := data.msgBlock.Header.BlockHash()

	if blockchain.HashToBig(&hash).Cmp(blockchain.CompactToBig(data.msgBlock.Header.Bits)) <= 0 {
		data.miner.submitBlock(exccutil.NewBlock(data.msgBlock))
		data.miner.minedOnParents[data.msgBlock.Header.PrevBlock]++
		*data.solved = true
		return 1
	}

	return 0
}

// solveAndSubmitBlock attempts to find some combination of a nonce, extra nonce, and
// current timestamp which makes the passed block hash to a value less than the
// target difficulty. After that, new block is submitted. The timestamp is
// updated periodically and the passed block is modified with all tweaks
// during this process. This means that when the function returns true, the block is submitted.
//
// This function will return early with false when conditions that trigger a
// stale block such as a new block showing up or periodically when there are
// new transactions and enough time has elapsed without finding a solution.
func (m *CPUMiner) solveAndSubmitBlock(msgBlock *wire.MsgBlock, ticker *time.Ticker, quit chan struct{}) bool {
	// Choose a random extra nonce offset for this block template and
	// worker.
	enOffset, err := wire.RandomUint64()
	if err != nil {
		minrLog.Errorf("Unexpected error while generating random extra nonce offset: %v", err)
		enOffset = 0
	}

	// Create a couple of convenience variables.
	header := &msgBlock.Header

	// Initial state.
	lastGenerated := time.Now()
	lastTxUpdate := m.txSource.LastUpdated()

	solved := false
	exiting := false
	validatorData := solutionValidatorData{&solved, &exiting, msgBlock, m, quit}

	// Note that the entire extra nonce range is iterated and the offset is
	// added relying on the fact that overflow will wrap around 0 as
	// provided by the Go spec.
	for extraNonce := uint64(0); extraNonce < maxExtraNonce && !solved && !exiting; extraNonce++ {
		// Update the extra nonce in the block template header with the
		// new value.
		littleEndian.PutUint64(header.ExtraData[:], extraNonce+enOffset)

		// Update equihash solver input bytes
		headerBytes, _ := header.SerializeAllHeaderBytes()

		// Search through the entire nonce range for a solution while
		// periodically checking for early quit and stale block
		// conditions along with updates to the speed monitor.
		for i := uint32(0); i <= maxNonce && !solved && !exiting; i++ {
			select {
			case <-quit:
				minrLog.Infof("Miner is stopping")
				exiting = true
				return false

			case <-ticker.C:
				minrLog.Debugf("Miner is updating time for currently mined block")

				// The current block is stale if the memory pool
				// has been updated since the block template was
				// generated and it has been at least 3 seconds,
				// or if it's been one minute.
				if (lastTxUpdate != m.txSource.LastUpdated() &&
					time.Now().After(lastGenerated.Add(3*time.Second))) ||
					time.Now().After(lastGenerated.Add(60*time.Second)) {

					return false
				}

				err = UpdateBlockTime(msgBlock, m.server.blockManager)

				if err != nil {
					minrLog.Warnf("CPU miner unable to update block template time: %v", err)
					return false
				}

				// Rebuild all input data
				headerBytes, err = header.SerializeAllHeaderBytes()

				if err != nil {
					minrLog.Warnf("CPU miner unable to rebuild header data for updated block template "+
						"time: %v", err)
					return false
				}

			default:
				// Non-blocking select to fall through
			}

			header.Nonce = i
			equihash.SolveEquihash(m.server.chainParams.N, m.server.chainParams.K, headerBytes, int64(i), validatorData)
		}
	}

	return solved
}

// generateBlocks is a worker that is controlled by the miningWorkerController.
// It is self contained in that it creates block templates and attempts to solve
// them while detecting when it is performing stale work and reacting
// accordingly by generating a new block template.  When a block is solved, it
// is submitted.
//
// It must be run as a goroutine.
func (m *CPUMiner) generateBlocks(quit chan struct{}) {
	minrLog.Tracef("Starting generate blocks worker")

	// Start a ticker which is used to signal checks for stale work and
	// updates to the speed monitor.
	ticker := time.NewTicker(333 * time.Millisecond)
	defer ticker.Stop()

out:
	for {
		// Quit when the miner is stopped.
		select {
		case <-quit:
			break out
		default:
			// Non-blocking select to fall through
		}

		// No point in searching for a solution before the chain is
		// synced.  Also, grab the same lock as used for block
		// submission, since the current block will be changing and
		// this would otherwise end up building a new block template on
		// a block that is in the process of becoming stale.
		m.submitBlockLock.Lock()
		time.Sleep(100 * time.Millisecond)

		// Hacks to make exccd work with ExchangeCoin PoC (simnet only)
		// TODO Remove before production.
		if cfg.SimNet {
			_, curHeight := m.server.blockManager.chainState.Best()

			if curHeight == 1 {
				time.Sleep(5500 * time.Millisecond) // let wallet reconn
			} else if curHeight > 100 && curHeight < 201 { // slow down to i
				time.Sleep(10 * time.Millisecond) // 2500
			} else { // burn through the first pile of blocks
				time.Sleep(10 * time.Millisecond)
			}
		}

		payToAddr, err := m.server.blockManager.GetMiningAddr()
		if err != nil {
			minrLog.Errorf("Failed to get mining address: %v", err)
			continue
		}

		// Create a new block template using the available transactions
		// in the memory pool as a source of transactions to potentially
		// include in the block.
		template, err := NewBlockTemplate(m.policy, m.server, payToAddr)
		m.submitBlockLock.Unlock()
		if err != nil {
			errStr := fmt.Sprintf("Failed to create new block template: %v", err)
			minrLog.Errorf(errStr)
			continue
		}

		// Not enough voters.
		if template == nil {
			continue
		}

		// This prevents you from causing memory exhaustion issues
		// when mining aggressively in a simulation network.
		if cfg.SimNet {
			if m.minedOnParents[template.Block.Header.PrevBlock] >=
				maxSimnetToMine {
				minrLog.Tracef("too many blocks mined on parent, stopping " +
					"until there are enough votes on these to make a new block")
				continue
			}
		}

		// Attempt to solve the block and submit solution.
		// The function will exit early with false when conditions
		// that trigger a stale block, so a new block template can be generated.
		m.solveAndSubmitBlock(template.Block, ticker, quit)
	}

	m.workerWg.Done()
	minrLog.Tracef("Generate blocks worker done")
}

// miningWorkerController launches the worker goroutines that are used to
// generate block templates and solve them.  It also provides the ability to
// dynamically adjust the number of running worker goroutines.
//
// It must be run as a goroutine.
func (m *CPUMiner) miningWorkerController() {
	// launchWorkers groups common code to launch a specified number of
	// workers for generating blocks.
	var runningWorkers []chan struct{}
	launchWorkers := func(numWorkers uint32) {
		for i := uint32(0); i < numWorkers; i++ {
			quit := make(chan struct{})
			runningWorkers = append(runningWorkers, quit)

			m.workerWg.Add(1)
			go m.generateBlocks(quit)
		}
	}

	// Launch the current number of workers by default.
	runningWorkers = make([]chan struct{}, 0, m.numWorkers)
	launchWorkers(m.numWorkers)

out:
	for {
		select {
		// Update the number of running workers.
		case <-m.updateNumWorkers:
			// No change.
			numRunning := uint32(len(runningWorkers))
			if m.numWorkers == numRunning {
				continue
			}

			// Add new workers.
			if m.numWorkers > numRunning {
				launchWorkers(m.numWorkers - numRunning)
				continue
			}

			// Signal the most recently created goroutines to exit.
			for i := numRunning - 1; i >= m.numWorkers; i-- {
				close(runningWorkers[i])
				runningWorkers[i] = nil
				runningWorkers = runningWorkers[:i]
			}

		case <-m.quit:
			for _, quit := range runningWorkers {
				close(quit)
			}
			break out
		}
	}

	// Wait until all workers shut down to stop the speed monitor since
	// they rely on being able to send updates to it.
	m.workerWg.Wait()
	close(m.speedMonitorQuit)
	m.wg.Done()
}

// Start begins the CPU mining process as well as the speed monitor used to
// track hashing metrics.  Calling this function when the CPU miner has
// already been started will have no effect.
//
// This function is safe for concurrent access.
func (m *CPUMiner) Start() {
	m.Lock()
	defer m.Unlock()

	// Nothing to do if the miner is already running or if running in discrete
	// mode (using GenerateNBlocks).
	if m.started || m.discreteMining {
		return
	}

	m.quit = make(chan struct{})
	m.speedMonitorQuit = make(chan struct{})
	m.wg.Add(2)
	go m.speedMonitor()
	go m.miningWorkerController()

	m.started = true
	minrLog.Infof("CPU miner started")
}

// Stop gracefully stops the mining process by signalling all workers, and the
// speed monitor to quit.  Calling this function when the CPU miner has not
// already been started will have no effect.
//
// This function is safe for concurrent access.
func (m *CPUMiner) Stop() {
	m.Lock()
	defer m.Unlock()

	// Nothing to do if the miner is not currently running or if running in
	// discrete mode (using GenerateNBlocks).
	if !m.started || m.discreteMining {
		return
	}

	close(m.quit)
	m.wg.Wait()
	m.started = false
	minrLog.Infof("CPU miner stopped")
}

// IsMining returns whether or not the CPU miner has been started and is
// therefore currenting mining.
//
// This function is safe for concurrent access.
func (m *CPUMiner) IsMining() bool {
	m.Lock()
	defer m.Unlock()

	return m.started
}

// HashesPerSecond returns the number of hashes per second the mining process
// is performing.  0 is returned if the miner is not currently running.
//
// This function is safe for concurrent access.
func (m *CPUMiner) HashesPerSecond() float64 {
	m.Lock()
	defer m.Unlock()

	// Nothing to do if the miner is not currently running.
	if !m.started {
		return 0
	}

	return <-m.queryHashesPerSec
}

// SetNumWorkers sets the number of workers to create which solve blocks.  Any
// negative values will cause a default number of workers to be used which is
// based on the number of processor cores in the system.  A value of 0 will
// cause all CPU mining to be stopped.
//
// This function is safe for concurrent access.
func (m *CPUMiner) SetNumWorkers(numWorkers int32) {
	if numWorkers == 0 {
		m.Stop()
	}

	// Don't lock until after the first check since Stop does its own
	// locking.
	m.Lock()
	defer m.Unlock()

	// Use default if provided value is negative.
	if numWorkers < 0 {
		m.numWorkers = defaultNumWorkers
	} else {
		m.numWorkers = uint32(numWorkers)
	}

	// When the miner is already running, notify the controller about the
	// the change.
	if m.started {
		m.updateNumWorkers <- struct{}{}
	}
}

// NumWorkers returns the number of workers which are running to solve blocks.
//
// This function is safe for concurrent access.
func (m *CPUMiner) NumWorkers() int32 {
	m.Lock()
	defer m.Unlock()

	return int32(m.numWorkers)
}

// GenerateNBlocks generates the requested number of blocks. It is self
// contained in that it creates block templates and attempts to solve them while
// detecting when it is performing stale work and reacting accordingly by
// generating a new block template.  When a block is solved, it is submitted.
// The function returns a list of the hashes of generated blocks.
func (m *CPUMiner) GenerateNBlocks(n uint32) ([]*chainhash.Hash, error) {
	m.Lock()

	// Respond with an error if there's virtually 0 chance of CPU-mining a block.
	if !m.server.chainParams.GenerateSupported {
		m.Unlock()
		return nil, errors.New("no support for `generate` on the current network, " +
			m.server.chainParams.Net.String() + ", as it's unlikely to be possible to CPU-mine a block.")
	}

	// Respond with an error if server is already mining.
	if m.started || m.discreteMining {
		m.Unlock()
		return nil, errors.New("server is already CPU mining. Please call " +
			"`setgenerate 0` before calling discrete `generate` commands")
	}

	m.started = true
	m.discreteMining = true

	m.speedMonitorQuit = make(chan struct{})
	m.wg.Add(1)
	go m.speedMonitor()

	m.Unlock()

	minrLog.Tracef("Generating %d blocks", n)

	i := uint32(0)
	blockHashes := make([]*chainhash.Hash, n)

	// Start a ticker which is used to signal checks for stale work and
	// updates to the speed monitor.
	ticker := time.NewTicker(time.Second * hashUpdateSecs)
	defer ticker.Stop()

	for {
		// Read updateNumWorkers in case someone tries a `setgenerate` while
		// we're generating. We can ignore it as the `generate` RPC call only
		// uses 1 worker.
		select {
		case <-m.updateNumWorkers:
		default:
		}

		// Grab the lock used for block submission, since the current block will
		// be changing and this would otherwise end up building a new block
		// template on a block that is in the process of becoming stale.
		m.submitBlockLock.Lock()

		payToAddr, err := m.server.blockManager.GetMiningAddr()
		if err != nil {
			minrLog.Errorf("Failed to get mining address: %v", err)
			continue
		}

		// Create a new block template using the available transactions
		// in the memory pool as a source of transactions to potentially
		// include in the block.
		template, err := NewBlockTemplate(m.policy, m.server, payToAddr)
		m.submitBlockLock.Unlock()
		if err != nil {
			errStr := fmt.Sprintf("Failed to create new block template: %v", err)
			minrLog.Errorf(errStr)
			continue
		}
		if template == nil {
			errStr := fmt.Sprintf("Not enough voters on parent block and failed to pull parent template")
			minrLog.Debugf(errStr)
			continue
		}

		// Attempt to solve the block.  The function will exit early
		// with false when conditions that trigger a stale block, so
		// a new block template can be generated.  When the return is
		// true a solution was found, so submit the solved block.
		if m.solveAndSubmitBlock(template.Block, ticker, nil) {
			blockHashes[i] = exccutil.NewBlock(template.Block).Hash()
			i++

			if i == n {
				minrLog.Tracef("Generated %d blocks", i)
				m.Lock()
				close(m.speedMonitorQuit)
				m.wg.Wait()
				m.started = false
				m.discreteMining = false
				m.Unlock()
				return blockHashes, nil
			}
		}
	}
}

// newCPUMiner returns a new instance of a CPU miner for the provided server.
// Use Start to begin the mining process.  See the documentation for CPUMiner
// type for more details.
func newCPUMiner(policy *mining.Policy, s *server) *CPUMiner {
	return &CPUMiner{
		policy:            policy,
		txSource:          s.txMemPool,
		server:            s,
		numWorkers:        defaultNumWorkers,
		updateNumWorkers:  make(chan struct{}),
		queryHashesPerSec: make(chan float64),
		updateHashes:      make(chan uint64),
		minedOnParents:    make(map[chainhash.Hash]uint8),
	}
}
