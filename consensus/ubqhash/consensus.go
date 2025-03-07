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

package ubqhash

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/ubiq/go-ubiq/v5/common"
	"github.com/ubiq/go-ubiq/v5/consensus"
	"github.com/ubiq/go-ubiq/v5/core/state"
	"github.com/ubiq/go-ubiq/v5/core/types"
	"github.com/ubiq/go-ubiq/v5/log"
	"github.com/ubiq/go-ubiq/v5/params"
	"github.com/ubiq/go-ubiq/v5/rlp"
	"github.com/ubiq/go-ubiq/v5/trie"
	"golang.org/x/crypto/sha3"
)

// Ubqhash proof-of-work protocol constants.
var (
	maxUncles              = 2                // Maximum number of uncles allowed in a single block
	allowedFutureBlockTime = 15 * time.Second // Max time from current time allowed for blocks, before they're considered future blocks
)

// Diff algo constants.
var (
	big88 = big.NewInt(88)

	digishieldV3Config = &diffConfig{
		AveragingWindow: big.NewInt(21),
		MaxAdjustDown:   big.NewInt(16), // 16%
		MaxAdjustUp:     big.NewInt(8),  // 8%
		Factor:          big.NewInt(100),
	}

	digishieldV3ModConfig = &diffConfig{
		AveragingWindow: big.NewInt(88),
		MaxAdjustDown:   big.NewInt(3), // 3%
		MaxAdjustUp:     big.NewInt(2), // 2%
		Factor:          big.NewInt(100),
	}

	fluxConfig = &diffConfig{
		AveragingWindow: big.NewInt(88),
		MaxAdjustDown:   big.NewInt(5), // 0.5%
		MaxAdjustUp:     big.NewInt(3), // 0.3%
		Dampen:          big.NewInt(1), // 0.1%
		Factor:          big.NewInt(1000),
	}
)

type diffConfig struct {
	AveragingWindow *big.Int `json:"averagingWindow"`
	MaxAdjustDown   *big.Int `json:"maxAdjustDown"`
	MaxAdjustUp     *big.Int `json:"maxAdjustUp"`
	Dampen          *big.Int `json:"dampen,omitempty"`
	Factor          *big.Int `json:"factor"`
}

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	errZeroBlockTime     = errors.New("timestamp equals parent's")
	errTooManyUncles     = errors.New("too many uncles")
	errDuplicateUncle    = errors.New("duplicate uncle")
	errUncleIsAncestor   = errors.New("uncle is ancestor")
	errDanglingUncle     = errors.New("uncle's parent is not ancestor")
	errInvalidDifficulty = errors.New("non-positive difficulty")
	errInvalidMixDigest  = errors.New("invalid mix digest")
	errInvalidPoW        = errors.New("invalid proof-of-work")
)

// Author implements consensus.Engine, returning the header's coinbase as the
// proof-of-work verified author of the block.
func (ubqhash *Ubqhash) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

// VerifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum ubqhash engine.
func (ubqhash *Ubqhash) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	// If we're running a full engine faking, accept any input as valid
	if ubqhash.config.PowMode == ModeFullFake {
		return nil
	}
	// Short circuit if the header is known, or it's parent not
	number := header.Number.Uint64()
	if chain.GetHeader(header.Hash(), number) != nil {
		return nil
	}
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	// Sanity checks passed, do a proper verification
	return ubqhash.verifyHeader(chain, header, parent, false, seal)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications.
func (ubqhash *Ubqhash) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	// If we're running a full engine faking, accept any input as valid
	if ubqhash.config.PowMode == ModeFullFake || len(headers) == 0 {
		abort, results := make(chan struct{}), make(chan error, len(headers))
		for i := 0; i < len(headers); i++ {
			results <- nil
		}
		return abort, results
	}

	// Spawn as many workers as allowed threads
	workers := runtime.GOMAXPROCS(0)
	if len(headers) < workers {
		workers = len(headers)
	}

	// Create a task channel and spawn the verifiers
	var (
		inputs = make(chan int)
		done   = make(chan int, workers)
		errors = make([]error, len(headers))
		abort  = make(chan struct{})
	)
	for i := 0; i < workers; i++ {
		go func() {
			for index := range inputs {
				errors[index] = ubqhash.verifyHeaderWorker(chain, headers, seals, index)
				done <- index
			}
		}()
	}

	errorsOut := make(chan error, len(headers))
	go func() {
		defer close(inputs)
		var (
			in, out = 0, 0
			checked = make([]bool, len(headers))
			inputs  = inputs
		)
		for {
			select {
			case inputs <- in:
				if in++; in == len(headers) {
					// Reached end of headers. Stop sending to workers.
					inputs = nil
				}
			case index := <-done:
				for checked[index] = true; checked[out]; out++ {
					errorsOut <- errors[out]
					if out == len(headers)-1 {
						return
					}
				}
			case <-abort:
				return
			}
		}
	}()
	return abort, errorsOut
}

func (ubqhash *Ubqhash) verifyHeaderWorker(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool, index int) error {
	var parent *types.Header
	if index == 0 {
		parent = chain.GetHeader(headers[0].ParentHash, headers[0].Number.Uint64()-1)
	} else if headers[index-1].Hash() == headers[index].ParentHash {
		parent = headers[index-1]
	}
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	if chain.GetHeader(headers[index].Hash(), headers[index].Number.Uint64()) != nil {
		return nil // known block
	}
	return ubqhash.verifyHeader(chain, headers[index], parent, false, seals[index])
}

// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of the stock Ethereum ubqhash engine.
func (ubqhash *Ubqhash) VerifyUncles(chain consensus.ChainHeaderReader, block *types.Block) error {
	// If we're running a full engine faking, accept any input as valid
	if ubqhash.config.PowMode == ModeFullFake {
		return nil
	}
	// Verify that there are at most 2 uncles included in this block
	if len(block.Uncles()) > maxUncles {
		return errTooManyUncles
	}
	// Gather the set of past uncles and ancestors
	uncles, ancestors := mapset.NewSet(), make(map[common.Hash]*types.Header)

	number, parent := block.NumberU64()-1, block.ParentHash()
	for i := 0; i < 7; i++ {
		ancestor := chain.GetBlock(parent, number)
		if ancestor == nil {
			break
		}
		ancestors[ancestor.Hash()] = ancestor.Header()
		for _, uncle := range ancestor.Uncles() {
			uncles.Add(uncle.Hash())
		}
		parent, number = ancestor.ParentHash(), number-1
	}
	ancestors[block.Hash()] = block.Header()
	uncles.Add(block.Hash())

	// Verify each of the uncles that it's recent, but not an ancestor
	for _, uncle := range block.Uncles() {
		// Make sure every uncle is rewarded only once
		hash := uncle.Hash()
		if uncles.Contains(hash) {
			return errDuplicateUncle
		}
		uncles.Add(hash)

		// Make sure the uncle has a valid ancestry
		if ancestors[hash] != nil {
			return errUncleIsAncestor
		}
		if ancestors[uncle.ParentHash] == nil || uncle.ParentHash == block.ParentHash() {
			return errDanglingUncle
		}
		if err := ubqhash.verifyHeader(chain, uncle, ancestors[uncle.ParentHash], true, true); err != nil {
			return err
		}
	}
	return nil
}

// verifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum ubqhash engine.
// See YP section 4.3.4. "Block Header Validity"
func (ubqhash *Ubqhash) verifyHeader(chain consensus.ChainHeaderReader, header, parent *types.Header, uncle bool, seal bool) error {
	// Ensure that the header's extra-data section is of a reasonable size
	if uint64(len(header.Extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra-data too long: %d > %d", len(header.Extra), params.MaximumExtraDataSize)
	}
	// Verify the header's timestamp
	if !uncle {
		if header.Time > uint64(time.Now().Add(allowedFutureBlockTime).Unix()) {
			return consensus.ErrFutureBlock
		}
	}
	if header.Time <= parent.Time {
		return errZeroBlockTime
	}
	// Verify the block's difficulty based in it's timestamp and parent's difficulty
	expected := ubqhash.CalcDifficulty(chain, header.Time, parent)

	if expected.Cmp(header.Difficulty) != 0 {
		return fmt.Errorf("invalid difficulty: have %v, want %v", header.Difficulty, expected)
	}
	// Verify that the gas limit is <= 2^63-1
	cap := uint64(0x7fffffffffffffff)
	if header.GasLimit > cap {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, cap)
	}
	// Verify that the gasUsed is <= gasLimit
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}

	// Verify that the gas limit remains within allowed bounds
	diff := int64(parent.GasLimit) - int64(header.GasLimit)
	if diff < 0 {
		diff *= -1
	}
	limit := parent.GasLimit / params.GasLimitBoundDivisor

	if uint64(diff) >= limit || header.GasLimit < params.MinGasLimit {
		return fmt.Errorf("invalid gas limit: have %d, want %d += %d", header.GasLimit, parent.GasLimit, limit)
	}
	// Verify that the block number is parent's +1
	if diff := new(big.Int).Sub(header.Number, parent.Number); diff.Cmp(big.NewInt(1)) != 0 {
		return consensus.ErrInvalidNumber
	}
	// Verify the engine specific seal securing the block
	if seal {
		if err := ubqhash.VerifySeal(chain, header); err != nil {
			return err
		}
	}
	return nil
}

// Difficulty timespans
func averagingWindowTimespan(config *diffConfig) *big.Int {
	x := new(big.Int)
	return x.Mul(config.AveragingWindow, big88)
}

func minActualTimespan(config *diffConfig, dampen bool) *big.Int {
	x := new(big.Int)
	y := new(big.Int)
	z := new(big.Int)
	if dampen {
		x.Sub(config.Factor, config.Dampen)
		y.Mul(averagingWindowTimespan(config), x)
		z.Div(y, config.Factor)
	} else {
		x.Sub(config.Factor, config.MaxAdjustUp)
		y.Mul(averagingWindowTimespan(config), x)
		z.Div(y, config.Factor)
	}
	return z
}

func maxActualTimespan(config *diffConfig, dampen bool) *big.Int {
	x := new(big.Int)
	y := new(big.Int)
	z := new(big.Int)
	if dampen {
		x.Add(config.Factor, config.Dampen)
		y.Mul(averagingWindowTimespan(config), x)
		z.Div(y, config.Factor)
	} else {
		x.Add(config.Factor, config.MaxAdjustDown)
		y.Mul(averagingWindowTimespan(config), x)
		z.Div(y, config.Factor)
	}
	return z
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have when created at time given the parent block's time
// and difficulty.
func (ubqhash *Ubqhash) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return CalcDifficulty(chain, time, parent)
}

// CalcDifficulty determines which difficulty algorithm to use for calculating a new block
func CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	parentTime := parent.Time
	parentNumber := parent.Number
	parentDiff := parent.Difficulty

	config := chain.Config()
	ubqhashConfig := config.Ubqhash

	if parentNumber.Cmp(ubqhashConfig.FluxBlock) < 0 {
		if parentNumber.Cmp(ubqhashConfig.DigishieldModBlock) < 0 {
			// Original DigishieldV3
			return calcDifficultyDigishieldV3(chain, parentNumber, parentDiff, parent, digishieldV3Config)
		}
		// Modified DigishieldV3
		return calcDifficultyDigishieldV3(chain, parentNumber, parentDiff, parent, digishieldV3ModConfig)
	}
	// Flux
	return calcDifficultyFlux(chain, big.NewInt(int64(time)), big.NewInt(int64(parentTime)), parentNumber, parentDiff, parent)
}

// calcDifficultyDigishieldV3 is the original difficulty adjustment algorithm.
// It returns the difficulty that a new block should have when created at time
// given the parent block's time and difficulty.
// Based on Digibyte's Digishield v3 retargeting
func calcDifficultyDigishieldV3(chain consensus.ChainHeaderReader, parentNumber, parentDiff *big.Int, parent *types.Header, digishield *diffConfig) *big.Int {
	// holds intermediate values to make the algo easier to read & audit
	x := new(big.Int)
	nFirstBlock := new(big.Int)
	nFirstBlock.Sub(parentNumber, digishield.AveragingWindow)

	log.Debug(fmt.Sprintf("CalcDifficulty parentNumber: %v parentDiff: %v", parentNumber, parentDiff))

	// Check we have enough blocks
	if parentNumber.Cmp(digishield.AveragingWindow) < 1 {
		log.Debug(fmt.Sprintf("CalcDifficulty: parentNumber(%+x) < digishieldV3Config.AveragingWindow(%+x)", parentNumber, digishield.AveragingWindow))
		x.Set(parentDiff)
		return x
	}

	// Limit adjustment step
	// Use medians to prevent time-warp attacks
	nLastBlockTime := chain.CalcPastMedianTime(parentNumber.Uint64(), parent)
	nFirstBlockTime := chain.CalcPastMedianTime(nFirstBlock.Uint64(), parent)
	nActualTimespan := new(big.Int)
	nActualTimespan.Sub(nLastBlockTime, nFirstBlockTime)
	log.Debug(fmt.Sprintf("CalcDifficulty nActualTimespan = %v before dampening", nActualTimespan))

	y := new(big.Int)
	y.Sub(nActualTimespan, averagingWindowTimespan(digishield))
	y.Div(y, big.NewInt(4))
	nActualTimespan.Add(y, averagingWindowTimespan(digishield))
	log.Debug(fmt.Sprintf("CalcDifficulty nActualTimespan = %v before bounds", nActualTimespan))

	if nActualTimespan.Cmp(minActualTimespan(digishield, false)) < 0 {
		nActualTimespan.Set(minActualTimespan(digishield, false))
		log.Debug("CalcDifficulty Minimum Timespan set")
	} else if nActualTimespan.Cmp(maxActualTimespan(digishield, false)) > 0 {
		nActualTimespan.Set(maxActualTimespan(digishield, false))
		log.Debug("CalcDifficulty Maximum Timespan set")
	}

	log.Debug(fmt.Sprintf("CalcDifficulty nActualTimespan = %v final\n", nActualTimespan))

	// Retarget
	x.Mul(parentDiff, averagingWindowTimespan(digishield))
	log.Debug(fmt.Sprintf("CalcDifficulty parentDiff * AveragingWindowTimespan: %v", x))

	x.Div(x, nActualTimespan)
	log.Debug(fmt.Sprintf("CalcDifficulty x / nActualTimespan: %v", x))

	if x.Cmp(params.MinimumDifficulty) < 0 {
		x.Set(params.MinimumDifficulty)
	}

	return x
}

func calcDifficultyFlux(chain consensus.ChainHeaderReader, time, parentTime, parentNumber, parentDiff *big.Int, parent *types.Header) *big.Int {
	x := new(big.Int)
	nFirstBlock := new(big.Int)
	nFirstBlock.Sub(parentNumber, fluxConfig.AveragingWindow)

	// Check we have enough blocks
	if parentNumber.Cmp(fluxConfig.AveragingWindow) < 1 {
		log.Debug(fmt.Sprintf("CalcDifficulty: parentNumber(%+x) < fluxConfig.AveragingWindow(%+x)", parentNumber, fluxConfig.AveragingWindow))
		x.Set(parentDiff)
		return x
	}

	diffTime := new(big.Int)
	diffTime.Sub(time, parentTime)

	nLastBlockTime := chain.CalcPastMedianTime(parentNumber.Uint64(), parent)
	nFirstBlockTime := chain.CalcPastMedianTime(nFirstBlock.Uint64(), parent)
	nActualTimespan := new(big.Int)
	nActualTimespan.Sub(nLastBlockTime, nFirstBlockTime)

	y := new(big.Int)
	y.Sub(nActualTimespan, averagingWindowTimespan(fluxConfig))
	y.Div(y, big.NewInt(4))
	nActualTimespan.Add(y, averagingWindowTimespan(fluxConfig))

	if nActualTimespan.Cmp(minActualTimespan(fluxConfig, false)) < 0 {
		doubleBig88 := new(big.Int)
		doubleBig88.Mul(big88, big.NewInt(2))
		if diffTime.Cmp(doubleBig88) > 0 {
			nActualTimespan.Set(minActualTimespan(fluxConfig, true))
		} else {
			nActualTimespan.Set(minActualTimespan(fluxConfig, false))
		}
	} else if nActualTimespan.Cmp(maxActualTimespan(fluxConfig, false)) > 0 {
		halfBig88 := new(big.Int)
		halfBig88.Div(big88, big.NewInt(2))
		if diffTime.Cmp(halfBig88) < 0 {
			nActualTimespan.Set(maxActualTimespan(fluxConfig, true))
		} else {
			nActualTimespan.Set(maxActualTimespan(fluxConfig, false))
		}
	}

	x.Mul(parentDiff, averagingWindowTimespan(fluxConfig))
	x.Div(x, nActualTimespan)

	if x.Cmp(params.MinimumDifficulty) < 0 {
		x.Set(params.MinimumDifficulty)
	}

	return x
}

// VerifySeal implements consensus.Engine, checking whether the given block satisfies
// the PoW difficulty requirements.
func (ubqhash *Ubqhash) VerifySeal(chain consensus.ChainHeaderReader, header *types.Header) error {
	return ubqhash.verifySeal(chain, header, false)
}

// verifySeal checks whether a block satisfies the PoW difficulty requirements,
// either using the usual ethash cache for it, or alternatively using a full DAG
// to make remote mining fast.
func (ubqhash *Ubqhash) verifySeal(chain consensus.ChainHeaderReader, header *types.Header, fulldag bool) error {
	// If we're running a fake PoW, accept any seal as valid
	if ubqhash.config.PowMode == ModeFake || ubqhash.config.PowMode == ModeFullFake {
		time.Sleep(ubqhash.fakeDelay)
		if ubqhash.fakeFail == header.Number.Uint64() {
			return errInvalidPoW
		}
		return nil
	}
	// If we're running a shared PoW, delegate verification to it
	if ubqhash.shared != nil {
		return ubqhash.shared.verifySeal(chain, header, fulldag)
	}
	// Ensure that we have a valid difficulty for the block
	if header.Difficulty.Sign() <= 0 {
		return errInvalidDifficulty
	}
	// Recompute the digest and PoW values
	number := header.Number.Uint64()

	var (
		digest []byte
		result []byte
	)
	// If fast-but-heavy PoW verification was requested, use an ethash dataset
	if fulldag {
		dataset := ubqhash.dataset(number, true)
		if dataset.generated() {
			digest, result = hashimotoFull(dataset.dataset, ubqhash.SealHash(header).Bytes(), header.Nonce.Uint64())

			// Datasets are unmapped in a finalizer. Ensure that the dataset stays alive
			// until after the call to hashimotoFull so it's not unmapped while being used.
			runtime.KeepAlive(dataset)
		} else {
			// Dataset not yet generated, don't hang, use a cache instead
			fulldag = false
		}
	}
	// If slow-but-light PoW verification was requested (or DAG not yet ready), use an ethash cache
	if !fulldag {
		cache := ubqhash.cache(number)

		size := datasetSize(number)
		if ubqhash.config.PowMode == ModeTest {
			size = 32 * 1024
		}
		digest, result = hashimotoLight(size, cache.cache, ubqhash.SealHash(header).Bytes(), header.Nonce.Uint64())

		// Caches are unmapped in a finalizer. Ensure that the cache stays alive
		// until after the call to hashimotoLight so it's not unmapped while being used.
		runtime.KeepAlive(cache)
	}
	// Verify the calculated values against the ones provided in the header
	if !bytes.Equal(header.MixDigest[:], digest) {
		return errInvalidMixDigest
	}
	target := new(big.Int).Div(two256, header.Difficulty)
	if new(big.Int).SetBytes(result).Cmp(target) > 0 {
		return errInvalidPoW
	}
	return nil
}

// Prepare implements consensus.Engine, initializing the difficulty field of a
// header to conform to the ubqhash protocol. The changes are done inline.
func (ubqhash *Ubqhash) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Difficulty = ubqhash.CalcDifficulty(chain, header.Time, parent)
	return nil
}

// Finalize implements consensus.Engine, accumulating the block and uncle rewards,
// setting the final state and assembling the block.
func (ubqhash *Ubqhash) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header) {
	// Accumulate any block and uncle rewards and commit the final state root
	accumulateRewards(chain.Config(), state, header, uncles)
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
}

// FinalizeAndAssemble implements consensus.Engine, accumulating the block and
// uncle rewards, setting the final state and assembling the block.
func (ubqhash *Ubqhash) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	// Accumulate any block and uncle rewards and commit the final state root
	accumulateRewards(chain.Config(), state, header, uncles)
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))

	// Header seems complete, assemble into a block and return
	return types.NewBlock(header, txs, uncles, receipts, new(trie.Trie)), nil
}

// Some weird constants to avoid constant memory allocs for them.
var (
	big2  = big.NewInt(2)
	big32 = big.NewInt(32)
)

// SealHash returns the hash of a block prior to it being sealed.
func (ubqhash *Ubqhash) SealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()

	rlp.Encode(hasher, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra,
	})
	hasher.Sum(hash[:0])
	return hash
}

// CalcBaseBlockReward calculates the base block reward as per the ubiq monetary policy.
func CalcBaseBlockReward(config *params.UbqhashConfig, height *big.Int) (*big.Int, *big.Int) {
	reward := new(big.Int)

	for _, step := range config.MonetaryPolicy {
		if height.Cmp(step.Block) > 0 {
			reward = new(big.Int).Set(step.Reward)
		} else {
			break
		}
	}

	return new(big.Int).Set(config.MonetaryPolicy[0].Reward), reward
}

// CalcUncleBlockReward calculates the uncle miner reward based on depth.
func CalcUncleBlockReward(config *params.ChainConfig, blockHeight *big.Int, uncleHeight *big.Int, blockReward *big.Int) *big.Int {
	reward := new(big.Int)
	// calculate reward based on depth
	reward.Add(uncleHeight, big2)
	reward.Sub(reward, blockHeight)
	reward.Mul(reward, blockReward)
	reward.Div(reward, big2)

	// negative uncle reward fix. (activates along-side EIP158)
	if config.IsEIP158(blockHeight) && reward.Cmp(big.NewInt(0)) < 0 {
		reward = big.NewInt(0)
	}
	return reward
}

// AccumulateRewards credits the coinbase of the given block with the mining
// reward. The total reward consists of the static block reward and rewards for
// included uncles. The coinbase of each uncle block is also rewarded.
func accumulateRewards(config *params.ChainConfig, state *state.StateDB, header *types.Header, uncles []*types.Header) {
	// block reward (miner)
	initialReward, currentReward := CalcBaseBlockReward(config.Ubqhash, header.Number)

	// Uncle reward step down fix. (activates along-side byzantium)
	ufixReward := initialReward
	if config.IsByzantium(header.Number) {
		ufixReward = currentReward
	}

	for _, uncle := range uncles {
		// uncle block miner reward (depth === 1 ? baseBlockReward * 0.5 : 0)
		uncleReward := CalcUncleBlockReward(config, header.Number, uncle.Number, ufixReward)
		// update uncle miner balance
		state.AddBalance(uncle.Coinbase, uncleReward)
		// include uncle bonus reward (baseBlockReward/32)
		uncleReward.Div(ufixReward, big32)
		currentReward.Add(currentReward, uncleReward)
	}
	// update block miner balance
	state.AddBalance(header.Coinbase, currentReward)
}
