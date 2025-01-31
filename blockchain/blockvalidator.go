// Copyright (c) 2019 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package blockchain

import (
	"bytes"
	"context"
	"sort"
	"sync"

	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-address/address"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/blockchain/block"
	"github.com/iotexproject/iotex-core/pkg/log"
	"github.com/iotexproject/iotex-core/state/factory"
)

// Validator is the interface of validator
type Validator interface {
	// Validate validates the given block's content
	Validate(ctx context.Context, block *block.Block) error
	// AddActionEnvelopeValidators add validators
	AddActionEnvelopeValidators(...protocol.ActionEnvelopeValidator)

	// SetActPool set ActPoolManager
	SetActPool(actPool ActPoolManager)
}

type validator struct {
	sf                       factory.Factory
	validatorAddr            string
	actionEnvelopeValidators []protocol.ActionEnvelopeValidator
	senderBlackList          map[string]bool
	actPool                  ActPoolManager
}

var (
	// ErrInvalidTipHeight is the error returned when the block height is not valid
	ErrInvalidTipHeight = errors.New("invalid tip height")
	// ErrInvalidBlock is the error returned when the block is not valid
	ErrInvalidBlock = errors.New("failed to validate the block")
	// ErrActionNonce is the error when the nonce of the action is wrong
	ErrActionNonce = errors.New("invalid action nonce")
	// ErrInsufficientGas indicates the error of insufficient gas value for data storage
	ErrInsufficientGas = errors.New("insufficient intrinsic gas value")
	// ErrBalance indicates the error of balance
	ErrBalance = errors.New("invalid balance")
)

// Validate validates the given block's content
func (v *validator) Validate(ctx context.Context, blk *block.Block) error {
	bcCtx := protocol.MustGetBlockchainCtx(ctx)
	if err := verifyHeightAndHash(blk, bcCtx.Tip.Height, bcCtx.Tip.Hash); err != nil {
		return errors.Wrap(err, "failed to verify block's height and hash")
	}
	if err := verifySigAndRoot(blk); err != nil {
		return errors.Wrap(err, "failed to verify block's signature and merkle root")
	}

	if v.sf == nil {
		return nil
	}

	return v.validateActionsOnly(ctx, blk)
}

// AddActionEnvelopeValidators add action envelope validators
func (v *validator) AddActionEnvelopeValidators(validators ...protocol.ActionEnvelopeValidator) {
	v.actionEnvelopeValidators = append(v.actionEnvelopeValidators, validators...)
}

// SetActPool set ActPool
func (v *validator) SetActPool(actPool ActPoolManager) {
	v.actPool = actPool
}

func (v *validator) validateActionsOnly(
	ctx context.Context,
	blk *block.Block,
) error {
	actions := blk.Actions
	// Verify transfers, votes, executions, witness, and secrets
	errChan := make(chan error, len(actions))
	accountNonceMap := make(map[string][]uint64)

	if err := v.validateActions(
		ctx,
		actions,
		accountNonceMap,
		errChan,
	); err != nil {
		close(errChan)
		return err
	}

	close(errChan)
	for err := range errChan {
		return errors.Wrap(err, "failed to validate action")
	}

	// Special handling for genesis block
	if blk.Height() == 0 {
		return nil
	}
	// Verify each account's Nonce
	for srcAddr, receivedNonces := range accountNonceMap {
		confirmedNonce, err := v.sf.Nonce(srcAddr)
		if err != nil {
			return errors.Wrapf(err, "failed to get the confirmed nonce of address %s", srcAddr)
		}
		receivedNonces := receivedNonces
		sort.Slice(receivedNonces, func(i, j int) bool { return receivedNonces[i] < receivedNonces[j] })
		for i, nonce := range receivedNonces {
			if nonce != confirmedNonce+uint64(i+1) {
				return errors.Wrapf(
					action.ErrNonce,
					"the %d nonce %d of address %s (confirmed nonce %d) is not continuously increasing",
					i,
					nonce,
					srcAddr,
					confirmedNonce,
				)
			}
		}
	}
	return nil
}

func (v *validator) validateActions(
	ctx context.Context,
	actions []action.SealedEnvelope,
	accountNonceMap map[string][]uint64,
	errChan chan error,
) error {
	var actionCtx protocol.ActionCtx
	bcCtx := protocol.MustGetBlockchainCtx(ctx)

	var wg sync.WaitGroup
	for _, selp := range actions {
		caller, err := address.FromBytes(selp.SrcPubkey().Hash())
		if err != nil {
			return err
		}
		if _, ok := v.senderBlackList[caller.String()]; ok {
			return errors.Wrap(action.ErrAddress, "action source address is blacklisted")
		}
		appendActionIndex(accountNonceMap, caller.String(), selp.Nonce())
		// not need validate action if it already exists in pool
		if v.actPool != nil {
			if _, err = v.actPool.GetActionByHash(selp.Hash()); err == nil {
				continue
			}
		}
		actionCtx.Caller = caller
		aCtx := protocol.WithActionCtx(ctx, actionCtx)

		for _, validator := range v.actionEnvelopeValidators {
			wg.Add(1)
			go func(validator protocol.ActionEnvelopeValidator, selp action.SealedEnvelope) {
				defer wg.Done()
				if err := validator.Validate(aCtx, selp); err != nil {
					errChan <- err
					return
				}
			}(validator, selp)
		}

		for _, validator := range bcCtx.Registry.All() {
			wg.Add(1)
			go func(validator protocol.ActionValidator, act action.Action) {
				defer wg.Done()
				if err := validator.Validate(aCtx, act); err != nil {
					errChan <- err
					return
				}
			}(validator, selp.Action())
		}
	}
	wg.Wait()

	return nil
}

func verifyHeightAndHash(blk *block.Block, tipHeight uint64, tipHash hash.Hash256) error {
	if blk == nil {
		return ErrInvalidBlock
	}
	// verify new block has height incremented by 1
	if blk.Height() != 0 && blk.Height() != tipHeight+1 {
		return errors.Wrapf(
			ErrInvalidTipHeight,
			"wrong block height %d, expecting %d",
			blk.Height(),
			tipHeight+1)
	}
	// verify new block has correctly linked to current tip
	if blk.PrevHash() != tipHash {
		blk.HeaderLogger(log.L()).Error("Previous block hash doesn't match.",
			log.Hex("expectedBlockHash", tipHash[:]))
		return errors.Wrapf(
			ErrInvalidBlock,
			"wrong prev hash %x, expecting %x",
			blk.PrevHash(),
			tipHash)
	}
	return nil
}

func verifySigAndRoot(blk *block.Block) error {
	if blk.Height() > 0 {
		// verify new block's signature is correct
		if !blk.VerifySignature() {
			return errors.Wrapf(
				ErrInvalidBlock,
				"failed to verify block's signature with public key: %x",
				blk.PublicKey())
		}
	}

	hashExpect := blk.TxRoot()
	hashActual := blk.CalculateTxRoot()
	if !bytes.Equal(hashExpect[:], hashActual[:]) {
		return errors.Wrapf(
			ErrInvalidBlock,
			"wrong tx hash %x, expecting %x",
			hashActual,
			hashExpect)
	}
	return nil
}

func appendActionIndex(accountNonceMap map[string][]uint64, srcAddr string, nonce uint64) {
	if nonce == 0 {
		return
	}
	if _, ok := accountNonceMap[srcAddr]; !ok {
		accountNonceMap[srcAddr] = make([]uint64, 0)
	}
	accountNonceMap[srcAddr] = append(accountNonceMap[srcAddr], nonce)
}
