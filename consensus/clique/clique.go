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

// Package clique implements the proof-of-authority consensus engine.
package clique

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/gochain-io/gochain/common/perfutils"

	"github.com/gochain-io/gochain/accounts"
	"github.com/gochain-io/gochain/common"
	"github.com/gochain-io/gochain/common/hexutil"
	"github.com/gochain-io/gochain/consensus"
	"github.com/gochain-io/gochain/consensus/misc"
	"github.com/gochain-io/gochain/core/types"
	"github.com/gochain-io/gochain/crypto"
	"github.com/gochain-io/gochain/crypto/sha3"
	"github.com/gochain-io/gochain/ethdb"
	"github.com/gochain-io/gochain/log"
	"github.com/gochain-io/gochain/params"
	"github.com/gochain-io/gochain/rlp"
	"github.com/gochain-io/gochain/rpc"
	lru "github.com/hashicorp/golang-lru"
)

const (
	checkpointInterval = 1024 // Number of blocks after which to save the vote snapshot to the database
	inmemorySnapshots  = 128  // Number of recent vote snapshots to keep in memory
	inmemorySignatures = 4096 // Number of recent block signatures to keep in memory

	wiggleTime = 500 * time.Millisecond // Random delay (per signer) to allow concurrent signers
)

// Clique proof-of-authority protocol constants.
var (
	epochLength = uint64(30000) // Default number of blocks after which to checkpoint and reset the pending votes
	blockPeriod = uint64(5)     // Default minimum difference between two consecutive block's timestamps

	signatureLength = 65

	extraVanity  = 32                       // Fixed number of extra-data prefix bytes reserved for signer vanity.
	extraPropose = common.AddressLength + 1 // Number of extra-data suffix bytes reserved for a proposal vote.

	voterElection  byte = 0xff
	signerElection byte = 0x00

	nonceAuthVote = hexutil.MustDecode("0xffffffffffffffff") // Magic nonce number to vote on adding a new signer
	nonceDropVote = hexutil.MustDecode("0x0000000000000000") // Magic nonce number to vote on removing a signer.

	uncleHash = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.

	diffInTurn = big.NewInt(2) // Block difficulty for in-turn signatures
	diffNoTurn = big.NewInt(1) // Block difficulty for out-of-turn signatures
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	// errUnknownBlock is returned when the list of signers is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errInvalidCheckpointBeneficiary is returned if a checkpoint/epoch transition
	// block has a beneficiary set to non-zeroes.
	errInvalidCheckpointBeneficiary = errors.New("beneficiary in checkpoint block non-zero")

	// errInvalidVote is returned if a nonce value is something else that the two
	// allowed constants of 0x00..0 or 0xff..f.
	errInvalidVote = errors.New("vote nonce not 0x00..0 or 0xff..f")

	// errInvalidCheckpointVote is returned if a checkpoint/epoch transition block
	// has a vote nonce set to non-zeroes.
	errInvalidCheckpointVote = errors.New("vote nonce in checkpoint block non-zero")

	// errMissingVanity is returned if a block's extra-data section is shorter than
	// 32 bytes, which is required to store the signer vanity.
	errMissingVanity = errors.New("extra-data 32 byte vanity prefix missing")

	// errMissingSignature is returned if a block's signer section doesn't seem
	// to contain a 65 byte secp256k1 signature.
	errMissingSignature = errors.New("signer signature missing")

	// errMissingSigners is returned if a block's signers section is empty
	errMissingSigners = errors.New("signers list missing")

	// errMissingVoters is returned if a block's voters section is empty
	errMissingVoters = errors.New("voters list missing")

	// errInvalidCheckpointSigners is returned if a checkpoint block contains an
	// invalid list of signers
	errInvalidCheckpointSigners = errors.New("invalid signer list on checkpoint block")

	// errInvalidCheckpointVoters is returned if a checkpoint block contains an
	// invalid list of voters
	errInvalidCheckpointVoters = errors.New("invalid voter list on checkpoint block")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")

	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	// errInvalidDifficulty is returned if the difficulty of a block is not either
	// of 1 or 2, or if the value does not match the turn of the signer.
	errInvalidDifficulty = errors.New("invalid difficulty")

	// ErrInvalidTimestamp is returned if the timestamp of a block is lower than
	// the previous block's timestamp + the minimum block period.
	ErrInvalidTimestamp = errors.New("invalid timestamp")

	// errInvalidVotingChain is returned if an authorization list is attempted to
	// be modified via out-of-range or non-contiguous headers.
	errInvalidVotingChain = errors.New("invalid voting chain")

	// errUnauthorized is returned if a header is signed by a non-authorized entity.
	errUnauthorized = errors.New("unauthorized")

	// errWaitTransactions is returned if an empty block is attempted to be sealed
	// on an instant chain (0 second period). It's important to refuse these as the
	// block reward is zero, so an empty block just bloats the chain... fast.
	errWaitTransactions = errors.New("waiting for transactions")
)

// SignerFn is a signer callback function to request a hash to be signed by a
// backing account.
type SignerFn func(accounts.Account, []byte) ([]byte, error)

// sigHash returns the hash which is used as input for the proof-of-authority
// signing. It is the hash of the entire header apart from the 65 byte signature
// contained at the end of the extra data.
//
// Note, the method requires the extra data to be at least 65 bytes, otherwise it
// panics. This is done to avoid accidentally using both forms (signature present
// or not), which could be abused to produce different hashes for the same header.
func sigHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewKeccak256SingleSum()

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
		header.Signers,
		header.Voters,
		header.Extra, // Yes, this will panic if extra is too short
		header.MixDigest,
		header.Nonce,
	})
	hasher.Sum(hash[:0])
	return hash
}

// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header, sigcache *lru.ARCCache) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()
	if address, known := sigcache.Get(hash); known {
		return address.(common.Address), nil
	}
	// Retrieve the signature from the header
	if len(header.Signer) < signatureLength {
		return common.Address{}, errMissingSignature
	}
	signature := header.Signer

	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	h := crypto.Keccak256Hash(pubkey[1:])
	var signer common.Address
	copy(signer[:], h[12:])

	sigcache.Add(hash, signer)
	return signer, nil
}

type propose struct {
	Authorize     bool
	VoterElection bool
}

// Clique is the proof-of-authority consensus engine proposed to support the
// Ethereum testnet following the Ropsten attacks.
type Clique struct {
	config *params.CliqueConfig // Consensus engine configuration parameters
	db     ethdb.Database       // Database to store and retrieve snapshot checkpoints

	recents    *lru.ARCCache // Snapshots for recent block to speed up reorgs
	signatures *lru.ARCCache // Signatures of recent blocks to speed up mining

	proposals map[common.Address]propose // Current list of proposals we are pushing

	signer common.Address // Ethereum address of the signing key
	signFn SignerFn       // Signer function to authorize hashes with
	lock   sync.RWMutex   // Protects the signer fields
}

// New creates a Clique proof-of-authority consensus engine with the initial
// signers set to the ones provided by the user.
func New(config *params.CliqueConfig, db ethdb.Database) *Clique {
	// Set any missing consensus parameters to their defaults
	conf := *config
	if conf.Epoch == 0 {
		conf.Epoch = epochLength
	}
	// Allocate the snapshot caches and create the engine
	recents, _ := lru.NewARC(inmemorySnapshots)
	signatures, _ := lru.NewARC(inmemorySignatures)

	return &Clique{
		config:     &conf,
		db:         db,
		recents:    recents,
		signatures: signatures,
		proposals:  make(map[common.Address]propose),
	}
}

// Author implements consensus.Engine, returning the Ethereum address recovered
// from the signature in the header's extra-data section.
func (c *Clique) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
	//return ecrecover(header, c.signatures)
}

// VerifyHeader checks whether a header conforms to the consensus rules.
func (c *Clique) VerifyHeader(ctx context.Context, chain consensus.ChainReader, header *types.Header, seal bool) error {
	return c.verifyHeader(ctx, chain, header, nil)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers. The
// method returns a quit channel to abort the operations and a results channel to
// retrieve the async verifications (the order is that of the input slice).
func (c *Clique) VerifyHeaders(ctx context.Context, chain consensus.ChainReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := c.verifyHeader(ctx, chain, header, headers[:i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
func (c *Clique) verifyHeader(ctx context.Context, chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	number := header.Number.Uint64()

	// Don't waste time checking blocks from the future
	if header.Time.Cmp(big.NewInt(time.Now().Unix())) > 0 {
		return consensus.ErrFutureBlock
	}
	// Checkpoint blocks need to enforce zero beneficiary
	checkpoint := (number % c.config.Epoch) == 0
	if checkpoint && len(header.Extra) > extraVanity {
		return errInvalidCheckpointBeneficiary
	}
	// Nonces must be 0x00..0 or 0xff..f, zeroes enforced on checkpoints
	if !bytes.Equal(header.Nonce[:], nonceAuthVote) && !bytes.Equal(header.Nonce[:], nonceDropVote) {
		return errInvalidVote
	}
	if checkpoint && !bytes.Equal(header.Nonce[:], nonceDropVote) {
		return errInvalidCheckpointVote
	}
	// Check that the extra-data contains the vanity
	//if len(header.Extra) < extraVanity {
	//	return errMissingVanity
	//}

	// Check if header contains signers and voters
	if checkpoint {
		if len(header.Signers) < 1 {
			return errMissingSigners
		}
		if len(header.Voters) < 1 {
			return errMissingVoters
		}
	}
	// Check if block was signed
	if len(header.Signer) < signatureLength {
		return errMissingSignature
	}
	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}
	// Ensure that the block doesn't contain any uncles which are meaningless in PoA
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}
	// Ensure that the block's difficulty is meaningful (may not be correct at this point)
	if number > 0 {
		if header.Difficulty == nil || (header.Difficulty.Cmp(diffInTurn) != 0 && header.Difficulty.Cmp(diffNoTurn) != 0) {
			return errInvalidDifficulty
		}
	}
	// If all checks passed, validate any special fields for hard forks
	if err := misc.VerifyForkHashes(chain.Config(), header, false); err != nil {
		return err
	}
	// All basic checks passed, verify cascading fields
	return c.verifyCascadingFields(ctx, chain, header, parents)
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers. The caller may optionally pass
// in a batch of parents (ascending order) to avoid looking those up from the
// database. This is useful for concurrently verifying a batch of new headers.
func (c *Clique) verifyCascadingFields(ctx context.Context, chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}
	// Ensure that the block's timestamp isn't too close to it's parent
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	if parent.Time.Uint64()+c.config.Period > header.Time.Uint64() {
		return ErrInvalidTimestamp
	}
	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := c.snapshot(ctx, chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}
	// If the block is a checkpoint block, verify the signer list
	if number%c.config.Epoch == 0 {
		for i, signer := range snap.signers() {
			if signer != header.Signers[i] {
				return errInvalidCheckpointSigners
			}
		}
		for i, voter := range snap.voters() {
			if voter != header.Voters[i] {
				return errInvalidCheckpointVoters
			}
		}
	}
	// All basic checks passed, verify the seal and return
	return c.verifySeal(ctx, chain, header, parents)
}

// snapshot retrieves the authorization snapshot at a given point in time.
func (c *Clique) snapshot(ctx context.Context, chain consensus.ChainReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	pt := perfutils.GetTimer(ctx)
	ps := pt.Start(perfutils.CliqueSnapshot)
	defer ps.Stop()
	// Search for a snapshot in memory or on disk for checkpoints
	var (
		headers []*types.Header
		snap    *Snapshot
	)
	for snap == nil {
		// If an in-memory snapshot was found, use that
		if s, ok := c.recents.Get(hash); ok {
			snap = s.(*Snapshot)
			break
		}
		// If an on-disk checkpoint snapshot can be found, use that
		if number%checkpointInterval == 0 {
			if s, err := loadSnapshot(c.config, c.signatures, c.db, hash); err == nil {
				log.Trace("Loaded voting snapshot form disk", "number", number, "hash", hash)
				snap = s
				break
			}
		}
		// If we're at block zero, make a snapshot
		if number == 0 {
			genesis := chain.GetHeaderByNumber(0)
			if err := c.VerifyHeader(ctx, chain, genesis, false); err != nil {
				return nil, err
			}
			snap = newSnapshot(c.config, c.signatures, 0, genesis.Hash(), genesis.Signers, genesis.Voters)
			if err := snap.store(c.db); err != nil {
				return nil, err
			}
			log.Trace("Stored genesis voting snapshot to disk")
			break
		}
		// No snapshot for this header, gather the header and move backward
		var header *types.Header
		if len(parents) > 0 {
			// If we have explicit parents, pick from there (enforced)
			header = parents[len(parents)-1]
			if header.Hash() != hash || header.Number.Uint64() != number {
				return nil, consensus.ErrUnknownAncestor
			}
			parents = parents[:len(parents)-1]
		} else {
			// No explicit parents (or no more left), reach out to the database
			header = chain.GetHeader(hash, number)
			if header == nil {
				return nil, consensus.ErrUnknownAncestor
			}
		}
		headers = append(headers, header)
		number, hash = number-1, header.ParentHash
	}
	// Previous snapshot found, apply any pending headers on top of it
	for i := 0; i < len(headers)/2; i++ {
		headers[i], headers[len(headers)-1-i] = headers[len(headers)-1-i], headers[i]
	}
	snap, err := snap.apply(headers)
	if err != nil {
		return nil, err
	}
	c.recents.Add(snap.Hash, snap)

	// If we've generated a new checkpoint snapshot, save to disk
	if snap.Number%checkpointInterval == 0 && len(headers) > 0 {
		if err = snap.store(c.db); err != nil {
			return nil, err
		}
		log.Trace("Stored voting snapshot to disk", "number", snap.Number, "hash", snap.Hash)
	}
	return snap, err
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (c *Clique) VerifyUncles(ctx context.Context, chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

// VerifySeal implements consensus.Engine, checking whether the signature contained
// in the header satisfies the consensus protocol requirements.
func (c *Clique) VerifySeal(ctx context.Context, chain consensus.ChainReader, header *types.Header) error {
	return c.verifySeal(ctx, chain, header, nil)
}

// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements. The method accepts an optional list of parent
// headers that aren't yet part of the local blockchain to generate the snapshots
// from.
func (c *Clique) verifySeal(ctx context.Context, chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	pt := perfutils.GetTimer(ctx)
	ps := pt.Start(perfutils.CliqueVerifySeal)
	defer ps.Stop()
	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}
	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := c.snapshot(ctx, chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}

	// Resolve the authorization key and check against signers
	signer, err := ecrecover(header, c.signatures)
	if err != nil {
		return err
	}
	if _, ok := snap.Signers[signer]; !ok {
		return errUnauthorized
	}
	for seen, recent := range snap.Recents {
		if recent == signer {
			// Signer is among recents, only fail if the current block doesn't shift it out
			if limit := uint64(len(snap.Signers)/2 + 1); seen > number-limit {
				return errUnauthorized
			}
		}
	}
	// Ensure that the difficulty corresponds to the turn-ness of the signer
	inturn := snap.inturn(header.Number.Uint64(), signer)
	if inturn && header.Difficulty.Cmp(diffInTurn) != 0 {
		return errInvalidDifficulty
	}
	if !inturn && header.Difficulty.Cmp(diffNoTurn) != 0 {
		return errInvalidDifficulty
	}
	return nil
}

// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
func (c *Clique) Prepare(ctx context.Context, chain consensus.ChainReader, header *types.Header) error {
	pt := perfutils.GetTimer(ctx)
	ps := pt.Start(perfutils.CliqueSeal)
	defer ps.Stop()
	// If the block isn't a checkpoint, cast a random vote (good enough for now)
	header.Nonce = types.BlockNonce{}

	number := header.Number.Uint64()
	// Assemble the voting snapshot to check which votes make sense
	snap, err := c.snapshot(ctx, chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}
	header.Extra = ExtraEnsureVanity(header.Extra)
	//if not checkpoint
	if number%c.config.Epoch != 0 {
		c.lock.RLock()

		// Gather all the proposals that make sense voting on
		addresses := make([]common.Address, 0, len(c.proposals))
		for address, propose := range c.proposals {
			if snap.validVote(address, propose.Authorize, propose.VoterElection) {
				addresses = append(addresses, address)
			}
		}
		// If there's pending proposals, cast a vote on them
		if len(addresses) > 0 {
			candidate := addresses[rand.Intn(len(addresses))]
			propose := c.proposals[candidate]
			header.Extra = ExtraAppendVote(header.Extra, candidate, propose.VoterElection)
			if propose.Authorize {
				copy(header.Nonce[:], nonceAuthVote)
			} else {
				copy(header.Nonce[:], nonceDropVote)
			}
			log.Info("propose", "Candidate", candidate, "vote", propose.Authorize, "voterElection", propose.VoterElection)
		}
		c.lock.RUnlock()
	}
	// Set the correct difficulty
	header.Difficulty = CalcDifficulty(snap, c.signer)

	if number%c.config.Epoch == 0 {
		header.Signers = snap.signers()
		header.Voters = snap.voters()
	}

	// Mix digest is reserved for now, set to empty
	header.MixDigest = common.Hash{}

	// Ensure the timestamp has the correct delay
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Time = new(big.Int).Add(parent.Time, new(big.Int).SetUint64(c.config.Period))
	if header.Time.Int64() < time.Now().Unix() {
		header.Time = big.NewInt(time.Now().Unix())
	}
	return nil
}

// Authorize injects a private key into the consensus engine to mint new blocks
// with.
func (c *Clique) Authorize(signer common.Address, signFn SignerFn) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.signer = signer
	c.signFn = signFn
}

// Seal implements consensus.Engine, attempting to create a sealed block using
// the local signing credentials.
func (c *Clique) Seal(ctx context.Context, chain consensus.ChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error) {
	pt := perfutils.GetTimer(ctx)
	ps := pt.Start(perfutils.CliqueSeal)
	defer ps.Stop()
	header := block.Header()

	// Sealing the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return nil, errUnknownBlock
	}
	// For 0-period chains, refuse to seal empty blocks (no reward but would spin sealing)
	if c.config.Period == 0 && len(block.Transactions()) == 0 {
		return nil, errWaitTransactions
	}
	// Don't hold the signer fields for the entire sealing procedure
	c.lock.RLock()
	signer, signFn := c.signer, c.signFn
	c.lock.RUnlock()

	// Bail out if we're unauthorized to sign a block
	snap, err := c.snapshot(ctx, chain, number-1, header.ParentHash, nil)
	if err != nil {
		return nil, err
	}
	if _, authorized := snap.Signers[signer]; !authorized {
		return nil, errUnauthorized
	}
	// If we're amongst the recent signers, wait for the next block
	limit := uint64(len(snap.Signers)/2 + 1)
	for seen, recent := range snap.Recents {
		if recent != signer {
			continue
		}
		if number >= seen+limit {
			continue
		}
		log.Info("Signed recently, must wait for others", "number", number, "signed", seen, "next", seen+limit)
		<-stop
		return nil, nil
	}
	// Sweet, the protocol permits us to sign the block, wait for our time
	delay := time.Unix(header.Time.Int64(), 0).Sub(time.Now()) // nolint: gosimple
	if header.Difficulty.Cmp(diffNoTurn) == 0 {
		// It's not our turn explicitly to sign, delay it a bit
		wiggle := time.Duration(len(snap.Signers)/2+1) * wiggleTime
		delay += time.Duration(rand.Int63n(int64(wiggle)))

		log.Trace("Out-of-turn signing requested", "wiggle", common.PrettyDuration(wiggle))
	}
	log.Trace("Waiting for slot to sign and propagate", "delay", common.PrettyDuration(delay))

	select {
	case <-stop:
		return nil, nil
	case <-time.After(delay):
	}
	// Sign all the things!
	sighash, err := signFn(accounts.Account{Address: signer}, sigHash(header).Bytes())
	if err != nil {
		return nil, err
	}
	header.Signer = sighash

	return block.WithSeal(header), nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have based on the previous blocks in the chain and the
// current signer.
func (c *Clique) CalcDifficulty(ctx context.Context, chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {
	snap, err := c.snapshot(ctx, chain, parent.Number.Uint64(), parent.Hash(), nil)
	if err != nil {
		return nil
	}
	return CalcDifficulty(snap, c.signer)
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have based on the previous blocks in the chain and the
// current signer.
func CalcDifficulty(snap *Snapshot, signer common.Address) *big.Int {
	if snap.inturn(snap.Number+1, signer) {
		return new(big.Int).Set(diffInTurn)
	}
	return new(big.Int).Set(diffNoTurn)
}

// APIs implements consensus.Engine, returning the user facing RPC API to allow
// controlling the signer voting.
func (c *Clique) APIs(chain consensus.ChainReader) []rpc.API {
	return []rpc.API{{
		Namespace: "clique",
		Version:   "1.0",
		Service:   &API{chain: chain, clique: c},
		Public:    false,
	}}
}

// ExtraEnsureVanity returns a slice of length 32, trimming extra or filling with 0s as necessary.
func ExtraEnsureVanity(extra []byte) []byte {
	if len(extra) < extraVanity {
		return append(extra, make([]byte, extraVanity-len(extra))...)
	}
	return extra[:extraVanity]
}

// ExtraVanity returns a slice of the vanity portion of extra (up to 32). It may still contain trailing 0s.
func ExtraVanity(extra []byte) []byte {
	if len(extra) < extraVanity {
		return extra
	}
	return extra[:extraVanity]
}

// ExtraAppendVote appends a vote to extra data as 20 bytes of address and a single byte for voter or signer election.
func ExtraAppendVote(extra []byte, candidate common.Address, voter bool) []byte {
	extra = append(extra, candidate[:]...)
	election := signerElection
	if voter {
		election = voterElection
	}
	return append(extra, election)
}

// ExtraHasVote returns true if extra contains a proposal vote.
func ExtraHasVote(extra []byte) bool {
	return len(extra) == extraVanity+extraPropose
}

// ExtraCandidate returns the candidate address of the proposal vote, or the zero value if one is not present.
func ExtraCandidate(extra []byte) common.Address {
	if len(extra) < extraVanity+common.AddressLength {
		return common.Address{}
	}
	return common.BytesToAddress(extra[extraVanity : extraVanity+common.AddressLength])
}

// IsVoterElection returns true if extra votes on a voter election, or false if it votes on a signer election or does
// not contain a vote.
func ExtraIsVoterElection(extra []byte) bool {
	if len(extra) < extraVanity+extraPropose {
		return false
	}
	switch extra[extraVanity+common.AddressLength] {
	case voterElection:
		return true
	case signerElection:
		return false
	}
	return false
}
