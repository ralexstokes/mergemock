package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"mergemock/api"
	"mergemock/p2p"
	"mergemock/rpc"
	"mergemock/types"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/prysmaticlabs/prysm/crypto/bls"
	"github.com/prysmaticlabs/prysm/crypto/bls/blst"
	"github.com/prysmaticlabs/prysm/runtime/version"
	"github.com/sirupsen/logrus"
)

type ConsensusCmd struct {
	BeaconGenesisTime uint64        `ask:"--beacon-genesis-time" help:"Beacon genesis time"`
	SlotTime          time.Duration `ask:"--slot-time" help:"Time per slot"`
	SlotsPerEpoch     uint64        `ask:"--slots-per-epoch" help:"Slots per epoch"`
	// TODO ideas:
	// - % random gap slots (= missing beacon blocks)
	// - % random finality

	EngineAddr    string `ask:"--engine" help:"Address of Engine JSON-RPC endpoint to use"`
	BuilderAddr   string `ask:"--builder" help:"Address of builder relay REST API endpoint to use"`
	DataDir       string `ask:"--datadir" help:"Directory to store execution chain data (empty for in-memory data)"`
	EthashDir     string `ask:"--ethashdir" help:"Directory to store ethash data"`
	GenesisPath   string `ask:"--genesis" help:"Genesis execution-config file"`
	JwtSecretPath string `ask:"--jwt-secret" help:"JWT secret key for authenticated communication"`
	Enode         string `ask:"--node" help:"Enode of execution client, required to insert pre-merge blocks."`
	SlotBound     uint64 `ask:"--slot-bound" help:"Terminate after the specified number of slots."`

	GenesisValidatorsRoot string `ask:"--genesis-validators-root" help:"Root of genesis validators"`

	// embed consensus behaviors
	ConsensusBehavior `ask:"."`

	// embed logger options
	LogCmd `ask:".log" help:"Change logger configuration"`

	TraceLogConfig `ask:".trace" help:"Tracing options"`

	close     chan struct{}
	log       logrus.Ext1FieldLogger
	ctx       context.Context
	engine    *rpc.Client
	jwtSecret []byte
	db        ethdb.Database

	genesisValidatorsRoot types.Root

	ethashCfg ethash.Config

	mockChain *MockChain
	sk        bls.SecretKey
}

func (c *ConsensusCmd) Default() {
	c.BeaconGenesisTime = uint64(time.Now().Unix()) + 5
	c.EngineAddr = "http://127.0.0.1:8551"
	c.GenesisPath = "genesis.json"
	c.JwtSecretPath = "jwt.hex"
	c.Enode = ""
	c.SlotBound = 0
	c.SlotTime = time.Second * 12
	c.SlotsPerEpoch = 32
	c.LogLvl = "info"
	c.GenesisValidatorsRoot = "0x0000000000000000000000000000000000000000000000000000000000000000"
}

func (c *ConsensusCmd) Help() string {
	return "Run a mock Consensus client."
}

func (c *ConsensusCmd) Run(ctx context.Context, args ...string) error {
	log, err := c.LogCmd.Create()
	if err != nil {
		return err
	}
	if c.SlotTime < 50*time.Millisecond {
		return fmt.Errorf("slot time %s is too small", c.SlotTime.String())
	}

	jwt, err := loadJwtSecret(c.JwtSecretPath)
	if err != nil {
		log.WithField("err", err).Fatal("Unable to read JWT secret")
	}
	c.jwtSecret = jwt
	log.WithField("val", common.Bytes2Hex(c.jwtSecret[:])).Info("Loaded JWT secret")

	c.genesisValidatorsRoot = types.Root(common.HexToHash(c.GenesisValidatorsRoot))

	// Connect to execution client engine api
	client, err := rpc.DialContext(ctx, c.EngineAddr, c.jwtSecret)
	if err != nil {
		return err
	}

	// Create a BLS key
	c.sk, err = blst.RandKey()
	if err != nil {
		return errors.New("unable to generate bls key pair")
	}

	c.ethashCfg = ethash.Config{
		PowMode:        ethash.ModeNormal,
		DatasetDir:     c.EthashDir,
		CacheDir:       c.EthashDir,
		DatasetsInMem:  1,
		DatasetsOnDisk: 2,
		CachesInMem:    2,
		CachesOnDisk:   3,
	}

	db, err := NewDB(c.DataDir)
	if err != nil {
		return fmt.Errorf("failed to open new db: %v", err)
	}

	c.log = log
	c.engine = client
	c.db = db
	c.ctx = ctx
	c.close = make(chan struct{})

	go c.RunNode()

	return nil
}

func (c *ConsensusCmd) SlotTimestamp(slot uint64) uint64 {
	return c.BeaconGenesisTime + uint64((time.Duration(slot) * c.SlotTime).Seconds())
}

func (c *ConsensusCmd) ValidateTimestamp(timestamp uint64, slot uint64) error {
	expectedTimestamp := c.BeaconGenesisTime + uint64((time.Duration(slot) * c.SlotTime).Seconds())
	if timestamp != expectedTimestamp {
		return fmt.Errorf("wrong timestamp: got %d, expected %d", timestamp, expectedTimestamp)
	}
	return nil
}

func (c *ConsensusCmd) proofOfWorkPrelogue(log logrus.Ext1FieldLogger) (transitionBlock uint64, err error) {
	// Create a temporary chain around the db, with ethash consensus, to run through the POW part.
	engine := ethash.New(c.ethashCfg, nil, false)

	mc, err := NewMockChain(log, engine, c.GenesisPath, c.db, &c.TraceLogConfig)
	if err != nil {
		return 0, fmt.Errorf("unable to initialize mock chain: %v", err)
	}
	if mc.chain.Config().TerminalTotalDifficulty.Cmp(common.Big0) != 1 {
		// Already transitioned
		return 0, nil
	}

	// Dial the peer to feed the POW blocks to
	n, err := enode.Parse(enode.ValidSchemes, c.Enode)
	if err != nil {
		return 0, fmt.Errorf("malformatted enode address (%q): %v", c.Enode, err)
	}
	peer, err := p2p.Dial(n)
	if err != nil {
		return 0, fmt.Errorf("unable to connect to client: %v", err)
	}
	if err := peer.Peer(mc.chain, nil); err != nil {
		return 0, fmt.Errorf("unable to peer with client: %v", err)
	}
	ctx, cancelPeer := context.WithCancel(c.ctx)
	defer cancelPeer()

	// keep peer connection alive until after the transition
	go peer.KeepAlive(ctx, log)

	defer mc.Close()
	defer engine.Close()

	// Send pre-transition blocks
	for {
		parent := mc.CurrentHeader()

		if c.RNG.Float64() < c.Freq.ReorgFreq {
			parent = c.calcReorgTarget(mc.chain, parent.Number.Uint64(), 0)
		}

		// build a block, without using the engine, and insert it into the engine
		block, err := mc.MineBlock(parent)
		if err != nil {
			return 0, fmt.Errorf("failed to mine block: %v", err)
		}

		// announce block
		newBlock := eth.NewBlockPacket{Block: block, TD: mc.CurrentTd()}
		if err := peer.Write66(&newBlock, 23); err != nil {
			return 0, fmt.Errorf("failed to msg peer: %v", err)
		}

		// check if terminal total difficulty is reached
		ttd := mc.chain.Config().TerminalTotalDifficulty
		td := mc.CurrentTd()
		log.WithField("td", td).WithField("ttd", ttd).Debug("Comparing TD to terminal TD")
		if td.Cmp(ttd) >= 0 {
			log.Info("Terminal total difficulty reached, transitioning to POS")
			return mc.CurrentHeader().Number.Uint64(), nil
		}
	}
}

func (c *ConsensusCmd) RunNode() {
	var (
		genesisTime     = time.Unix(int64(c.BeaconGenesisTime), 0)
		slots           = time.NewTicker(c.SlotTime)
		transitionBlock = uint64(0)
		finalizedHash   = common.Hash{}
		safeHash        = common.Hash{}
		nextFinalized   = common.Hash{}
		posEngine       = &ExecutionConsensusMock{
			pow: ethash.New(c.ethashCfg, nil, false),
			log: c.log,
		}
		payloadId = make(chan types.PayloadID)
	)
	defer slots.Stop()

	// Run PoW prelouge if peered with client
	if c.Enode != "" {
		var err error
		nr, err := c.proofOfWorkPrelogue(c.log.WithField("transitioned", false))
		if err != nil {
			c.log.WithField("err", err).Error("Failed to complete POW-prologue")
			os.Exit(1)
		}
		transitionBlock = nr
	} else {
		c.log.Info("No peer, skipping pre-merge transition simulation, starting in POS mode")
	}

	// Initialize mock chain with existing db
	mc, err := NewMockChain(c.log, posEngine, c.GenesisPath, c.db, &c.TraceLogConfig)
	if err != nil {
		c.log.WithField("err", err).Error("Unable to initialize mock chain")
		os.Exit(1)
	}
	c.mockChain = mc

	for {
		select {
		case tick := <-slots.C:
			signedSlot := int64(math.Round(float64(tick.Sub(genesisTime)) / float64(c.SlotTime)))
			if signedSlot < 0 {
				// before genesis...
				if signedSlot >= -10.0 {
					c.log.WithField("remaining_slots", -signedSlot).Info("Counting down to genesis...")
				}
				continue
			}
			if signedSlot == 0 {
				c.log.WithField("slot", 0).Info("Genesis!")
				safeHash = c.mockChain.CurrentHeader().Hash()
				continue
			}
			slot := uint64(signedSlot)
			if c.SlotBound > 0 && slot > c.SlotBound {
				c.log.WithField("testRuns", c.SlotBound).Info("All test runs successfully completed")
				os.Exit(0)
			}
			if slot%c.SlotsPerEpoch == 0 {
				last := finalizedHash
				finalizedHash = nextFinalized
				safeHash = finalizedHash
				nextFinalized = c.mockChain.CurrentHeader().Hash()
				c.log.WithField("slot", slot).WithField("last", last).WithField("new", finalizedHash).WithField("next", nextFinalized).Info("Finalized block updated")
			}
			// Gap slot
			if c.RNG.Float64() < c.Freq.GapSlot {
				c.log.WithField("slot", slot).Info("Mocking gap slot, no payload execution here")
				// empty pending proposal
				select {
				case <-payloadId:
				default:
				}
				continue
			}

			// Send bad hash
			if c.RNG.Float64() < c.Freq.InvalidHashFreq {
				c.log.Info("Sending payload with invalid hash")
				payload := &types.ExecutionPayloadV1{
					ParentHash:    c.mockChain.CurrentHeader().Hash(),
					FeeRecipient:  common.Address{},
					Number:        c.mockChain.CurrentHeader().Number.Uint64(),
					GasLimit:      c.mockChain.CurrentHeader().GasLimit,
					GasUsed:       0,
					Timestamp:     c.mockChain.CurrentHeader().Time + 1,
					BaseFeePerGas: c.mockChain.CurrentHeader().BaseFee,
					BlockHash:     common.HexToHash("0xdeadbeef"),
				}
				go api.NewPayloadV1(c.ctx, c.engine, c.log, payload)
				continue
			}

			// Fake some forking by building on an ancestor
			parent := c.mockChain.CurrentHeader()
			if c.RNG.Float64() < c.Freq.ReorgFreq {
				min := transitionBlock
				if final := c.mockChain.chain.GetHeaderByHash(finalizedHash); final != nil {
					num := final.Number.Uint64()
					if min < num {
						min = num
					}
				}
				parent = c.calcReorgTarget(c.mockChain.chain, parent.Number.Uint64(), min)
			}

			slotLog := c.log.WithField("slot", slot)
			slotLog.WithField("previous", parent.Hash()).Info("Slot trigger")

			// If we're proposing, get a block from the engine!
			select {
			case id := <-payloadId:
				slotLog.WithField("payloadId", id).Info("Update forkchoice to block built by engine")
				go c.mockProposal(slotLog, id, slot, false)
				continue
			default:
				// Not proposing a block
			}

			// Build a block, without using the engine, and insert it into the engine
			slotLog.Debug("Mocking external block")

			// TODO: different proposers, gas limit (target in london) changes, etc.
			coinbase := common.Address{1}
			timestamp := c.SlotTimestamp(slot)
			gasLimit := parent.GasLimit
			extraData := []byte("proto says hi")
			uncleBlocks := []*ethTypes.Header{}
			creator := TransactionsCreator{c.ConsensusBehavior.TestAccounts.accounts, dummyTxCreator}

			block, err := c.mockChain.AddNewBlock(parent.Hash(), coinbase, timestamp, gasLimit, creator, [32]byte{}, extraData, uncleBlocks, true)
			if err != nil {
				slotLog.WithError(err).Errorf("Failed to add block")
				continue
			}

			slotLog.WithField("blockhash", block.Hash()).Debug("Built external block")

			go func(log logrus.Ext1FieldLogger, block *ethTypes.Block, safe, final common.Hash) {
				c.mockExecution(log, block)
				latest := block.Hash()
				// Note: head and safe hash are set to the same hash,
				// until forkchoice updates are more attestation-weight aware.
				var attributes *types.PayloadAttributesV1
				if c.RNG.Float64() < c.Freq.ProposalFreq {
					// proposing next slot!
					attributes = c.makePayloadAttributes(slot + 1)
				}
				id, err := c.sendForkchoiceUpdated(latest, safe, final, attributes)
				if err != nil {
					maybeExit(c.SlotBound)
				}
				if id != nil {
					payloadId <- *id
				}
			}(slotLog, block, safeHash, finalizedHash)

		case <-c.close:
			c.log.Info("Closing consensus mock node")
			c.engine.Close()
			if err := c.mockChain.Close(); err != nil {
				c.log.WithError(err).Error("Failed closing mock chain")
			}
			if err := c.db.Close(); err != nil {
				c.log.WithError(err).Error("Failed closing database")
			}
		}
	}
}

func (c *ConsensusCmd) sendForkchoiceUpdated(latest, safe, final common.Hash, attributes *types.PayloadAttributesV1) (*types.PayloadID, error) {
	result, _ := api.ForkchoiceUpdatedV1(c.ctx, c.engine, c.log, latest, safe, final, attributes)
	if result.PayloadStatus.Status != types.ExecutionValid {
		c.log.WithField("status", result.PayloadStatus).Error("Update not considered valid")
		return nil, fmt.Errorf("update not considered valid")
	}
	return result.PayloadID, nil
}

func (c *ConsensusCmd) getMockProposal(ctx context.Context, log logrus.Ext1FieldLogger, payloadId types.PayloadID, slot uint64) (*types.ExecutionPayloadV1, error) {
	// If the CL is connected to builder client, request the payload from there.
	if c.BuilderAddr != "" {
		header, err := api.BuilderGetHeader(c.ctx, log, c.BuilderAddr, slot, c.mockChain.CurrentHeader().Hash(), c.sk.PublicKey().Marshal())
		if err != nil {
			return nil, err
		}

		signedBlindedBeaconBlock := &types.SignedBlindedBeaconBlock{
			Message: &types.BlindedBeaconBlock{
				Slot:          slot,
				ProposerIndex: 1,
				Body: &types.BlindedBeaconBlockBody{
					Eth1Data:               &types.Eth1Data{},
					SyncAggregate:          &types.SyncAggregate{},
					ExecutionPayloadHeader: header,
				},
			},
			Signature: types.Signature{},
		}
		domain := types.ComputeDomain(types.DomainTypeBeaconProposer, version.Bellatrix, &c.genesisValidatorsRoot)
		root, err := types.ComputeSigningRoot(signedBlindedBeaconBlock.Message, domain)
		if err != nil {
			return nil, err
		}
		sig := c.sk.Sign(root[:]).Marshal()
		signedBlindedBeaconBlock.Signature.FromSlice(sig)

		payload, err := api.BuilderGetPayload(ctx, log, c.sk, c.BuilderAddr, signedBlindedBeaconBlock)
		if err != nil {
			return nil, err
		}
		c.log.WithField("hash", payload.BlockHash.Hex()).Info("received payload from builder")
		return payload, err
	}

	// Otherwise, get payload from EL.
	payload, err := api.GetPayloadV1(c.ctx, c.engine, log, payloadId)
	if err != nil {
		return nil, err
	}
	return payload, err
}

func (c *ConsensusCmd) mockProposal(log logrus.Ext1FieldLogger, payloadId types.PayloadID, slot uint64, consensusFail bool) {
	ctx, cancel := context.WithTimeout(c.ctx, time.Second*20)
	defer cancel()

	payload, err := c.getMockProposal(ctx, log, payloadId, slot)
	if err != nil {
		log.WithError(err).Error("Unable to retrieve proposal payload")
		maybeExit(c.SlotBound)
		return
	}
	if err := c.ValidateTimestamp(uint64(payload.Timestamp), slot); err != nil {
		log.WithError(err).Error("Payload has bad timestamp")
		maybeExit(c.SlotBound)
		return
	}
	if consensusFail {
		log.Debug("Mocking a failed proposal on consensus-side, ignoring produced payload of engine")
		return
	}
	block, err := c.mockChain.ProcessPayload(payload)
	if err != nil {
		log.WithError(err).Error("Failed to process execution payload from engine")
		maybeExit(c.SlotBound)
		return
	} else {
		log.WithField("blockhash", block.Hash()).Debug("Processed payload in consensus mock world")
	}

	// Send it back to execution layer for execution
	res, err := api.NewPayloadV1(ctx, c.engine, log, payload)
	if err == nil && res.Status == types.ExecutionValid {
		log.WithField("blockhash", block.Hash()).Debug("Processed payload in engine")
		return
	}
	if err != nil {
		log.WithError(err).Error("Failed to execute payload")
	} else if res.Status == types.ExecutionInvalid {
		log.WithField("blockhash", block.Hash()).Error("Engine just produced payload and failed to execute it after!")
	} else {
		log.WithField("status", res.Status).Error("Unrecognized execution status")
	}
	maybeExit(c.SlotBound)
}

func (c *ConsensusCmd) mockExecution(log logrus.Ext1FieldLogger, block *ethTypes.Block) {
	ctx, cancel := context.WithTimeout(c.ctx, time.Second*20)
	defer cancel()

	// derive the random 32 bytes from the block hash for mocking ease
	payload, err := api.BlockToPayload(block)

	if err != nil {
		log.WithError(err).Error("Failed to convert execution block to execution payload")
		return
	}

	api.NewPayloadV1(ctx, c.engine, log, payload)
}

func dummyTxCreator(config *params.ChainConfig, bc core.ChainContext, statedb *state.StateDB, header *ethTypes.Header, cfg vm.Config, accounts []TestAccount) []*ethTypes.Transaction {
	// TODO create some more txs and use all accounts
	if len(accounts) != 0 {
		signer := ethTypes.NewLondonSigner(config.ChainID)
		txdata := &ethTypes.DynamicFeeTx{
			ChainID:   config.ChainID,
			Nonce:     statedb.GetNonce(accounts[0].addr),
			To:        &accounts[0].addr,
			Gas:       30000,
			GasFeeCap: new(big.Int).Mul(big.NewInt(5), big.NewInt(params.GWei)),
			GasTipCap: big.NewInt(2),
			Data:      []byte{},
		}
		tx := ethTypes.NewTx(txdata)
		tx, _ = ethTypes.SignTx(tx, signer, accounts[0].pk)
		return []*ethTypes.Transaction{tx}
	} else {
		return nil
	}
}

func (c *ConsensusCmd) calcReorgTarget(chain *core.BlockChain, parent uint64, min uint64) *ethTypes.Header {
	depth := c.RNG.Float64() * float64(c.ReorgMaxDepth)
	target := uint64(math.Max(float64(parent)-depth, float64(min)))
	return chain.GetHeaderByNumber(target)
}

func (c *ConsensusCmd) Close() error {
	if c.close != nil {
		c.close <- struct{}{}
	}
	return nil
}

func (c *ConsensusCmd) makePayloadAttributes(slot uint64) *types.PayloadAttributesV1 {
	var prevRandao common.Hash
	c.RNG.Read(prevRandao[:])
	return &types.PayloadAttributesV1{
		Timestamp:             c.SlotTimestamp(slot),
		PrevRandao:            prevRandao,
		SuggestedFeeRecipient: common.Address{0x13, 0x37},
	}
}

func maybeExit(val uint64) {
	if val != 0 {
		os.Exit(1)
	}
}
