// Copyright (c) 2019 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"fmt"
	"testing"

	"github.com/decred/dcrd/blockchain/v3/chaingen"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v2"
	"github.com/decred/dcrd/dcrutil/v3"
	"github.com/decred/dcrd/wire"
)

// TestProcessOrder ensures processing-specific logic such as orphan handling,
// duplicate block handling, and out-of-order reorgs to invalid blocks works as
// expected.
func TestProcessOrder(t *testing.T) {
	// Create a test harness initialized with the genesis block as the tip.
	params := chaincfg.RegNetParams()
	g, teardownFunc := newChaingenHarness(t, params, "processordertest")
	defer teardownFunc()

	// Define additional convenience helper function to process the current tip
	// block associated with the generator.
	//
	// orphaned expects the block to be accepted as an orphan.
	orphaned := func() {
		msgBlock := g.Tip()
		blockHeight := msgBlock.Header.Height
		block := dcrutil.NewBlock(msgBlock)
		t.Logf("Testing orphan block %s (hash %s, height %d)", g.TipName(),
			block.Hash(), blockHeight)

		forkLen, isOrphan, err := g.chain.ProcessBlock(block, BFNone)
		if err != nil {
			g.t.Fatalf("block %q (hash %s, height %d) not accepted: %v",
				g.TipName(), block.Hash(), blockHeight, err)
		}

		// Ensure the main chain and orphan flags match the values specified in
		// the test.
		isMainChain := !isOrphan && forkLen == 0
		if isMainChain {
			g.t.Fatalf("block %q (hash %s, height %d) unexpected main chain "+
				"flag -- got %v, want true", g.TipName(), block.Hash(),
				blockHeight, isMainChain)
		}
		if !isOrphan {
			g.t.Fatalf("block %q (hash %s, height %d) unexpected orphan flag "+
				"-- got %v, want false", g.TipName(), block.Hash(), blockHeight,
				isOrphan)
		}
	}

	// Shorter versions of useful params for convenience.
	coinbaseMaturity := params.CoinbaseMaturity
	stakeValidationHeight := params.StakeValidationHeight

	// ---------------------------------------------------------------------
	// Generate and accept enough blocks to reach stake validation height.
	// ---------------------------------------------------------------------

	g.AdvanceToStakeValidationHeight()

	// ---------------------------------------------------------------------
	// Generate enough blocks to have a known distance to the first mature
	// coinbase outputs for all tests that follow.  These blocks continue
	// to purchase tickets to avoid running out of votes.
	//
	//   ... -> bsv# -> bbm0 -> bbm1 -> ... -> bbm#
	// ---------------------------------------------------------------------

	for i := uint16(0); i < coinbaseMaturity; i++ {
		outs := g.OldestCoinbaseOuts()
		blockName := fmt.Sprintf("bbm%d", i)
		g.NextBlock(blockName, nil, outs[1:])
		g.SaveTipCoinbaseOuts()
		g.AcceptTipBlock()
	}
	g.AssertTipHeight(uint32(stakeValidationHeight) + uint32(coinbaseMaturity))

	// Collect spendable outputs into two different slices.  The outs slice
	// is intended to be used for regular transactions that spend from the
	// output, while the ticketOuts slice is intended to be used for stake
	// ticket purchases.
	var outs []*chaingen.SpendableOut
	var ticketOuts [][]chaingen.SpendableOut
	for i := uint16(0); i < coinbaseMaturity; i++ {
		coinbaseOuts := g.OldestCoinbaseOuts()
		outs = append(outs, &coinbaseOuts[0])
		ticketOuts = append(ticketOuts, coinbaseOuts[1:])
	}

	// Ensure duplicate blocks are rejected.
	//
	//   ... -> b1(0)
	//      \-> b1(0)
	g.NextBlock("b1", outs[0], ticketOuts[0])
	g.AcceptTipBlock()
	g.RejectTipBlock(ErrDuplicateBlock)

	// ---------------------------------------------------------------------
	// Orphan tests.
	// ---------------------------------------------------------------------

	// Create valid orphan block with zero prev hash.
	//
	//   No previous block
	//                    \-> borphan0(1)
	g.SetTip("b1")
	g.NextBlock("borphan0", outs[1], ticketOuts[1], func(b *wire.MsgBlock) {
		b.Header.PrevBlock = chainhash.Hash{}
	})
	orphaned()

	// Create valid orphan block.
	//
	//   ... -> b1(0)
	//               \-> borphanbase(1) -> borphan1(2)
	g.SetTip("b1")
	g.NextBlock("borphanbase", outs[1], ticketOuts[1])
	g.NextBlock("borphan1", outs[2], ticketOuts[2])
	orphaned()

	// Ensure duplicate orphan blocks are rejected.
	g.RejectTipBlock(ErrDuplicateBlock)

	// ---------------------------------------------------------------------
	// Out-of-order forked reorg to invalid block tests.
	// ---------------------------------------------------------------------

	// Create a fork that ends with block that generates too much proof-of-work
	// coinbase, but with a valid fork first.
	//
	//   ... -> b1(0) -> b2(1)
	//               \-> bpw1(1) -> bpw2(2) -> bpw3(3)
	//                  (bpw1 added last)
	g.SetTip("b1")
	g.NextBlock("b2", outs[1], ticketOuts[1])
	g.AcceptTipBlock()
	g.ExpectTip("b2")

	g.SetTip("b1")
	g.NextBlock("bpw1", outs[1], ticketOuts[1])
	g.NextBlock("bpw2", outs[2], ticketOuts[2])
	orphaned()
	g.NextBlock("bpw3", outs[3], ticketOuts[3], func(b *wire.MsgBlock) {
		// Increase the first proof-of-work coinbase subsidy.
		b.Transactions[0].TxOut[2].Value += 1
	})
	orphaned()
	g.RejectBlock("bpw1", ErrBadCoinbaseValue)
	g.ExpectTip("bpw2")

	// Create a fork that ends with block that generates too much dev-org
	// coinbase, but with a valid fork first.
	//
	//   ... -> b1(0) -> bpw1(1) -> bpw2(2)
	//                          \-> bdc1(2) -> bdc2(3) -> bdc3(4)
	//                             (bdc1 added last)
	g.SetTip("bpw1")
	g.NextBlock("bdc1", outs[2], ticketOuts[2])
	g.NextBlock("bdc2", outs[3], ticketOuts[3])
	orphaned()
	g.NextBlock("bdc3", outs[4], ticketOuts[4], func(b *wire.MsgBlock) {
		// Increase the proof-of-work dev subsidy by the provided amount.
		b.Transactions[0].TxOut[0].Value += 1
	})
	orphaned()
	g.RejectBlock("bdc1", ErrNoTax)
	g.ExpectTip("bdc2")
}
