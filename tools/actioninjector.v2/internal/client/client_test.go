package client

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/golang/protobuf/proto"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/stretchr/testify/require"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/api"
	"github.com/iotexproject/iotex-core/blockindex"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/db"
	"github.com/iotexproject/iotex-core/state"
	"github.com/iotexproject/iotex-core/test/identityset"
	"github.com/iotexproject/iotex-core/test/mock/mock_actpool"
	"github.com/iotexproject/iotex-core/test/mock/mock_blockchain"
	"github.com/iotexproject/iotex-core/test/mock/mock_factory"
	"github.com/iotexproject/iotex-core/testutil"
)

func TestClient(t *testing.T) {
	require := require.New(t)
	a := identityset.Address(28).String()
	priKeyA := identityset.PrivateKey(28)
	b := identityset.Address(29).String()

	cfg := config.Default
	cfg.API.Port = testutil.RandomPort()
	ctx := context.Background()

	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	state := state.EmptyAccount()
	chainID := uint32(1)
	tx, err := action.NewTransfer(uint64(1), big.NewInt(10), b, nil, uint64(0), big.NewInt(0))
	require.NoError(err)
	bd := &action.EnvelopeBuilder{}
	elp := bd.SetNonce(1).SetAction(tx).Build()
	selp, err := action.Sign(elp, priKeyA)
	require.NoError(err)

	bc := mock_blockchain.NewMockBlockchain(mockCtrl)
	sf := mock_factory.NewMockFactory(mockCtrl)
	ap := mock_actpool.NewMockActPool(mockCtrl)

	sf.EXPECT().AccountState(gomock.Any()).Return(&state, nil).AnyTimes()
	bc.EXPECT().Factory().Return(sf).AnyTimes()
	bc.EXPECT().ChainID().Return(chainID).AnyTimes()
	bc.EXPECT().AddSubscriber(gomock.Any()).Return(nil).AnyTimes()
	ap.EXPECT().GetPendingNonce(gomock.Any()).Return(uint64(1), nil).AnyTimes()
	ap.EXPECT().Add(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	newOption := api.WithBroadcastOutbound(func(_ context.Context, _ uint32, _ proto.Message) error {
		return nil
	})
	indexer, err := blockindex.NewIndexer(db.NewMemKVStore(), hash.ZeroHash256)
	require.NoError(err)
	apiServer, err := api.NewServer(cfg, bc, nil, indexer, ap, nil, newOption)
	require.NoError(err)
	require.NoError(apiServer.Start())
	// test New()
	serverAddr := fmt.Sprintf("127.0.0.1:%d", cfg.API.Port)
	cli, err := New(serverAddr, true)
	require.NoError(err)

	// test GetAccount()
	response, err := cli.GetAccount(ctx, a)
	require.NotNil(response)
	require.NoError(err)

	// test SendAction
	require.NoError(cli.SendAction(ctx, selp))
}
