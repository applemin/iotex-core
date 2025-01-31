// Copyright (c) 2019 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package rewarding

import (
	"context"
	"math/big"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/action/protocol/account"
	"github.com/iotexproject/iotex-core/action/protocol/rolldpos"
	"github.com/iotexproject/iotex-core/blockchain/genesis"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/db"
	"github.com/iotexproject/iotex-core/pkg/unit"
	"github.com/iotexproject/iotex-core/state"
	"github.com/iotexproject/iotex-core/test/identityset"
	"github.com/iotexproject/iotex-core/test/mock/mock_chainmanager"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
)

func testProtocol(t *testing.T, test func(*testing.T, context.Context, protocol.StateManager, *Protocol), withExempt bool) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	registry := protocol.NewRegistry()
	sm := mock_chainmanager.NewMockStateManager(ctrl)
	cb := db.NewCachedBatch()
	sm.EXPECT().State(gomock.Any(), gomock.Any()).DoAndReturn(
		func(addrHash hash.Hash160, account interface{}) error {
			val, err := cb.Get("state", addrHash[:])
			if err != nil {
				return state.ErrStateNotExist
			}
			return state.Deserialize(account, val)
		}).AnyTimes()
	sm.EXPECT().PutState(gomock.Any(), gomock.Any()).DoAndReturn(
		func(addrHash hash.Hash160, account interface{}) error {
			ss, err := state.Serialize(account)
			if err != nil {
				return err
			}
			cb.Put("state", addrHash[:], ss, "failed to put state")
			return nil
		}).AnyTimes()

	rp := rolldpos.NewProtocol(
		genesis.Default.NumCandidateDelegates,
		genesis.Default.NumDelegates,
		genesis.Default.NumSubEpochs,
	)
	p := NewProtocol(func(epochNum uint64) (uint64, map[string]uint64, error) {
		return uint64(19),
			map[string]uint64{
				identityset.Address(27).String(): 3,
				identityset.Address(28).String(): 7,
				identityset.Address(29).String(): 1,
				identityset.Address(30).String(): 6,
				identityset.Address(31).String(): 2,
			},
			nil
	}, rp)
	require.NoError(t, rp.Register(registry))
	require.NoError(t, p.Register(registry))

	candidates := []*state.Candidate{
		{
			Address:       identityset.Address(27).String(),
			Votes:         unit.ConvertIotxToRau(4000000),
			RewardAddress: identityset.Address(0).String(),
		},
		{
			Address:       identityset.Address(28).String(),
			Votes:         unit.ConvertIotxToRau(3000000),
			RewardAddress: identityset.Address(28).String(),
		},
		{
			Address:       identityset.Address(29).String(),
			Votes:         unit.ConvertIotxToRau(2000000),
			RewardAddress: identityset.Address(29).String(),
		},
		{
			Address:       identityset.Address(30).String(),
			Votes:         unit.ConvertIotxToRau(1000000),
			RewardAddress: identityset.Address(30).String(),
		},
		{
			Address:       identityset.Address(31).String(),
			Votes:         unit.ConvertIotxToRau(500000),
			RewardAddress: identityset.Address(31).String(),
		},
		{
			Address:       identityset.Address(32).String(),
			Votes:         unit.ConvertIotxToRau(500000),
			RewardAddress: identityset.Address(32).String(),
		},
	}
	ge := config.Default.Genesis
	// Create a test account with 1000 token
	ge.InitBalanceMap[identityset.Address(28).String()] = "1000"
	ge.Rewarding.InitBalanceStr = "0"
	ge.Rewarding.ExemptAddrStrsFromEpochReward = []string{}
	ge.Rewarding.BlockRewardStr = "10"
	ge.Rewarding.EpochRewardStr = "100"
	ge.Rewarding.NumDelegatesForEpochReward = 4
	ge.Rewarding.FoundationBonusStr = "5"
	ge.Rewarding.NumDelegatesForFoundationBonus = 5
	ge.Rewarding.FoundationBonusLastEpoch = 365
	ge.Rewarding.ProductivityThreshold = 50
	// Initialize the protocol
	if withExempt {
		ge.Rewarding.ExemptAddrStrsFromEpochReward = []string{
			identityset.Address(31).String(),
		}
		ge.Rewarding.NumDelegatesForEpochReward = 10
	}
	ctx := protocol.WithBlockCtx(
		context.Background(),
		protocol.BlockCtx{
			BlockHeight: 0,
		},
	)
	ctx = protocol.WithBlockchainCtx(
		ctx,
		protocol.BlockchainCtx{
			Genesis:    ge,
			Candidates: candidates,
		},
	)
	ap := account.NewProtocol(DepositGas)
	require.NoError(t, ap.Register(registry))
	require.NoError(t, ap.CreateGenesisStates(ctx, sm))
	require.NoError(t, p.CreateGenesisStates(ctx, sm))

	ctx = protocol.WithBlockCtx(
		ctx,
		protocol.BlockCtx{
			Producer:    identityset.Address(27),
			BlockHeight: genesis.Default.NumDelegates * genesis.Default.NumSubEpochs,
		},
	)
	ctx = protocol.WithActionCtx(
		ctx,
		protocol.ActionCtx{
			Caller: identityset.Address(28),
		},
	)
	ctx = protocol.WithBlockchainCtx(
		ctx,
		protocol.BlockchainCtx{
			Genesis:    ge,
			Registry:   registry,
			Candidates: candidates,
		},
	)
	blockReward, err := p.BlockReward(ctx, sm)
	require.NoError(t, err)
	assert.Equal(t, big.NewInt(10), blockReward)
	epochReward, err := p.EpochReward(ctx, sm)
	require.NoError(t, err)
	assert.Equal(t, big.NewInt(100), epochReward)
	fb, err := p.FoundationBonus(ctx, sm)
	require.NoError(t, err)
	assert.Equal(t, big.NewInt(5), fb)
	ndffb, err := p.NumDelegatesForFoundationBonus(ctx, sm)
	require.NoError(t, err)
	assert.Equal(t, uint64(5), ndffb)
	fble, err := p.FoundationBonusLastEpoch(ctx, sm)
	require.NoError(t, err)
	assert.Equal(t, uint64(365), fble)
	pt, err := p.ProductivityThreshold(ctx, sm)
	require.NoError(t, err)
	assert.Equal(t, uint64(50), pt)

	totalBalance, err := p.TotalBalance(ctx, sm)
	require.NoError(t, err)
	assert.Equal(t, big.NewInt(0), totalBalance)
	availableBalance, err := p.AvailableBalance(ctx, sm)
	require.NoError(t, err)
	assert.Equal(t, big.NewInt(0), availableBalance)

	test(t, ctx, sm, p)
}

func TestProtocol_Handle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cfg := config.Default
	registry := protocol.NewRegistry()
	sm := mock_chainmanager.NewMockStateManager(ctrl)
	cb := db.NewCachedBatch()
	sm.EXPECT().State(gomock.Any(), gomock.Any()).DoAndReturn(
		func(addrHash hash.Hash160, account interface{}) error {
			val, err := cb.Get("state", addrHash[:])
			if err != nil {
				return state.ErrStateNotExist
			}
			return state.Deserialize(account, val)
		}).AnyTimes()
	sm.EXPECT().PutState(gomock.Any(), gomock.Any()).DoAndReturn(
		func(addrHash hash.Hash160, account interface{}) error {
			ss, err := state.Serialize(account)
			if err != nil {
				return err
			}
			cb.Put("state", addrHash[:], ss, "failed to put state")
			return nil
		}).AnyTimes()
	sm.EXPECT().Snapshot().Return(1).AnyTimes()
	sm.EXPECT().Revert(gomock.Any()).Return(nil).AnyTimes()

	rp := rolldpos.NewProtocol(
		cfg.Genesis.NumCandidateDelegates,
		cfg.Genesis.NumDelegates,
		cfg.Genesis.NumSubEpochs,
	)
	require.NoError(t, rp.Register(registry))
	p := NewProtocol(func(epochNum uint64) (uint64, map[string]uint64, error) {
		return 0, nil, nil
	}, rp)
	require.NoError(t, p.Register(registry))
	cfg.Genesis.Rewarding.InitBalanceStr = "1000000"
	cfg.Genesis.Rewarding.BlockRewardStr = "10"
	cfg.Genesis.Rewarding.EpochRewardStr = "100"
	cfg.Genesis.Rewarding.NumDelegatesForEpochReward = 10
	cfg.Genesis.Rewarding.ExemptAddrStrsFromEpochReward = []string{}
	cfg.Genesis.Rewarding.FoundationBonusStr = "5"
	cfg.Genesis.Rewarding.NumDelegatesForFoundationBonus = 5
	cfg.Genesis.Rewarding.FoundationBonusLastEpoch = 0
	cfg.Genesis.Rewarding.ProductivityThreshold = 50
	// Create a test account with 1000000 token
	cfg.Genesis.InitBalanceMap[identityset.Address(0).String()] = "1000000"

	ctx := protocol.WithBlockCtx(
		context.Background(),
		protocol.BlockCtx{
			BlockHeight: 0,
		},
	)

	ctx = protocol.WithBlockchainCtx(
		ctx,
		protocol.BlockchainCtx{
			Genesis: cfg.Genesis,
			Candidates: []*state.Candidate{
				{
					Address:       identityset.Address(0).String(),
					Votes:         unit.ConvertIotxToRau(4000000),
					RewardAddress: identityset.Address(0).String(),
				},
			},
			Registry: registry,
		},
	)
	ap := account.NewProtocol(DepositGas)
	require.NoError(t, ap.Register(registry))
	require.NoError(t, ap.CreateGenesisStates(ctx, sm))
	require.NoError(t, p.CreateGenesisStates(ctx, sm))

	ctx = protocol.WithBlockCtx(
		ctx,
		protocol.BlockCtx{
			Producer:    identityset.Address(0),
			BlockHeight: genesis.Default.NumDelegates * genesis.Default.NumSubEpochs,
		},
	)
	ctx = protocol.WithActionCtx(
		ctx,
		protocol.ActionCtx{
			Caller:   identityset.Address(0),
			GasPrice: big.NewInt(0),
		},
	)

	// Deposit
	db := action.DepositToRewardingFundBuilder{}
	deposit := db.SetAmount(big.NewInt(1000000)).Build()
	eb1 := action.EnvelopeBuilder{}
	e1 := eb1.SetNonce(0).
		SetGasPrice(big.NewInt(0)).
		SetGasLimit(deposit.GasLimit()).
		SetAction(&deposit).
		Build()
	se1, err := action.Sign(e1, identityset.PrivateKey(0))
	require.NoError(t, err)

	receipt, err := p.Handle(ctx, se1.Action(), sm)
	require.NoError(t, err)
	balance, err := p.TotalBalance(ctx, sm)
	require.NoError(t, err)
	assert.Equal(t, big.NewInt(2000000), balance)

	// Grant
	gb := action.GrantRewardBuilder{}
	grant := gb.SetRewardType(action.BlockReward).Build()
	eb2 := action.EnvelopeBuilder{}
	e2 := eb2.SetNonce(0).
		SetGasPrice(big.NewInt(0)).
		SetGasLimit(grant.GasLimit()).
		SetAction(&grant).
		Build()
	se2, err := action.Sign(e2, identityset.PrivateKey(0))
	require.NoError(t, err)

	receipt, err = p.Handle(ctx, se2.Action(), sm)
	require.NoError(t, err)
	assert.Equal(t, uint64(iotextypes.ReceiptStatus_Success), receipt.Status)
	assert.Equal(t, 1, len(receipt.Logs))
	// Grant the block reward again should fail
	receipt, err = p.Handle(ctx, se2.Action(), sm)
	require.NoError(t, err)
	assert.Equal(t, uint64(iotextypes.ReceiptStatus_Failure), receipt.Status)
}
