// Copyright (c) 2019 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package e2etest

import (
	"context"
	"math/big"
	"testing"

	"github.com/iotexproject/go-pkgs/crypto"
	"github.com/stretchr/testify/require"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/action/protocol/account"
	"github.com/iotexproject/iotex-core/action/protocol/execution"
	"github.com/iotexproject/iotex-core/action/protocol/rewarding"
	"github.com/iotexproject/iotex-core/action/protocol/rolldpos"
	"github.com/iotexproject/iotex-core/blockchain"
	"github.com/iotexproject/iotex-core/blockchain/block"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/testutil"
)

const (
	executor       = "io1mflp9m6hcgm2qcghchsdqj3z3eccrnekx9p0ms"
	recipient      = "io1emxf8zzqckhgjde6dqd97ts0y3q496gm3fdrl6"
	executorPriKey = "cfa6ef757dee2e50351620dca002d32b9c090cfda55fb81f37f1d26b273743f1"
)

func TestTransfer_Negative(t *testing.T) {
	return
	r := require.New(t)
	ctx := context.Background()
	bc := prepareBlockchain(ctx, executor, r)
	defer r.NoError(bc.Stop(ctx))
	balanceBeforeTransfer, err := bc.Factory().Balance(executor)
	r.NoError(err)
	blk, err := prepareTransfer(bc, r)
	r.NoError(err)
	r.Error(bc.ValidateBlock(blk))
	r.Panics(func() { bc.CommitBlock(blk) })
	balance, err := bc.Factory().Balance(executor)
	r.NoError(err)
	r.Equal(0, balance.Cmp(balanceBeforeTransfer))
}

func TestAction_Negative(t *testing.T) {
	r := require.New(t)
	ctx := context.Background()
	bc := prepareBlockchain(ctx, executor, r)
	defer r.NoError(bc.Stop(ctx))
	balanceBeforeTransfer, err := bc.Factory().Balance(executor)
	r.NoError(err)
	blk, err := prepareAction(bc, r)
	r.NoError(err)
	r.NotNil(blk)
	r.Error(bc.ValidateBlock(blk))
	r.Panics(func() { bc.CommitBlock(blk) })
	balance, err := bc.Factory().Balance(executor)
	r.NoError(err)
	r.Equal(0, balance.Cmp(balanceBeforeTransfer))
}

func prepareBlockchain(ctx context.Context, executor string, r *require.Assertions) blockchain.Blockchain {
	cfg := config.Default
	cfg.Chain.EnableAsyncIndexWrite = false
	cfg.Genesis.EnableGravityChainVoting = false
	cfg.Genesis.InitBalanceMap[executor] = "1000000000000000000000000000"
	registry := protocol.NewRegistry()
	acc := account.NewProtocol(rewarding.DepositGas)
	r.NoError(acc.Register(registry))
	rp := rolldpos.NewProtocol(cfg.Genesis.NumCandidateDelegates, cfg.Genesis.NumDelegates, cfg.Genesis.NumSubEpochs)
	r.NoError(rp.Register(registry))
	bc := blockchain.NewBlockchain(
		cfg,
		nil,
		blockchain.InMemDaoOption(),
		blockchain.InMemStateFactoryOption(),
		blockchain.RegistryOption(registry),
	)
	r.NotNil(bc)
	reward := rewarding.NewProtocol(nil, rp)
	r.NoError(reward.Register(registry))

	bc.Validator().AddActionEnvelopeValidators(protocol.NewGenericValidator(bc.Factory().Nonce))
	sf := bc.Factory()
	r.NotNil(sf)
	r.NoError(bc.Start(ctx))
	ep := execution.NewProtocol(bc.BlockDAO().GetBlockHash)
	r.NoError(ep.Register(registry))
	r.NoError(bc.Start(ctx))
	return bc
}

func prepareTransfer(bc blockchain.Blockchain, r *require.Assertions) (*block.Block, error) {
	exec, err := action.NewTransfer(1, big.NewInt(-10000), recipient, nil, uint64(1000000), big.NewInt(9000000000000))
	r.NoError(err)
	builder := &action.EnvelopeBuilder{}
	elp := builder.SetAction(exec).
		SetNonce(exec.Nonce()).
		SetGasLimit(exec.GasLimit()).
		SetGasPrice(exec.GasPrice()).
		Build()
	return prepare(bc, elp, r)
}

func prepareAction(bc blockchain.Blockchain, r *require.Assertions) (*block.Block, error) {
	exec, err := action.NewExecution(action.EmptyAddress, 1, big.NewInt(-100), uint64(1000000), big.NewInt(9000000000000), []byte{})
	r.NoError(err)
	builder := &action.EnvelopeBuilder{}
	elp := builder.SetAction(exec).
		SetNonce(exec.Nonce()).
		SetGasLimit(exec.GasLimit()).
		SetGasPrice(exec.GasPrice()).
		Build()
	return prepare(bc, elp, r)
}

func prepare(bc blockchain.Blockchain, elp action.Envelope, r *require.Assertions) (*block.Block, error) {
	priKey, err := crypto.HexStringToPrivateKey(executorPriKey)
	r.NoError(err)
	selp, err := action.Sign(elp, priKey)
	r.NoError(err)
	actionMap := make(map[string][]action.SealedEnvelope)
	actionMap[executor] = []action.SealedEnvelope{selp}
	blk, err := bc.MintNewBlock(
		actionMap,
		testutil.TimestampNow(),
	)
	r.NoError(err)
	// when validate/commit a blk, the workingset and receipts of blk should be nil
	blk.WorkingSet = nil
	blk.Receipts = nil
	return blk, nil
}
