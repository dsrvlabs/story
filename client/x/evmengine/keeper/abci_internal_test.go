package keeper

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"cosmossdk.io/core/store"
	storetypes "cosmossdk.io/store/types"
	"cosmossdk.io/x/tx/signing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	cmttypes "github.com/cometbft/cometbft/types"
	cmttime "github.com/cometbft/cometbft/types/time"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/runtime"
	cosmosstd "github.com/cosmos/cosmos-sdk/std"
	"github.com/cosmos/cosmos-sdk/testutil"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authcodec "github.com/cosmos/cosmos-sdk/x/auth/codec"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	atypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	btypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	dtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/ethereum/go-ethereum"
	eengine "github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
	fuzz "github.com/google/gofuzz"
	"github.com/stretchr/testify/require"

	moduletestutil "github.com/piplabs/story/client/x/evmengine/testutil"
	etypes "github.com/piplabs/story/client/x/evmengine/types"
	"github.com/piplabs/story/lib/ethclient"
	"github.com/piplabs/story/lib/ethclient/mock"

	"go.uber.org/mock/gomock"
)

var zeroAddr common.Address

func TestKeeper_PrepareProposal(t *testing.T) {
	t.Parallel()

	// TestRunErrScenarios tests various error scenarios in the PrepareProposal function.
	// It covers cases where different errors are encountered during the preparation of a proposal,
	// such as when no transactions are provided, when errors occur while fetching block information,
	// or when errors occur during fork choice update.
	t.Run("TestRunErrScenarios", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name       string
			mockEngine mockEngineAPI
			mockClient mock.MockClient
			req        *abci.RequestPrepareProposal
			wantErr    bool
			setupMocks func(esk *moduletestutil.MockEvmStakingKeeper)
		}{
			{
				name:       "no transactions",
				mockEngine: mockEngineAPI{},
				mockClient: mock.MockClient{},
				req: &abci.RequestPrepareProposal{
					Txs:    nil,        // Set to nil to simulate no transactions
					Height: 1,          // Set height to 1 for this test case
					Time:   time.Now(), // Set time to current time or mock a time
				},
				wantErr: false,
			},
			{
				name:       "with transactions",
				mockEngine: mockEngineAPI{},
				mockClient: mock.MockClient{},
				req: &abci.RequestPrepareProposal{
					Txs:    [][]byte{[]byte("tx1")}, // simulate transactions
					Height: 1,
					Time:   time.Now(),
				},
				wantErr: true,
			},
			{
				name: "forkchoiceUpdateV2 not valid",
				mockEngine: mockEngineAPI{
					headerByTypeFunc: func(context.Context, ethclient.HeadType) (*types.Header, error) {
						fuzzer := ethclient.NewFuzzer(0)
						var header *types.Header
						fuzzer.Fuzz(&header)

						return header, nil
					},
					forkchoiceUpdatedV3Func: func(ctx context.Context, update eengine.ForkchoiceStateV1,
						payloadAttributes *eengine.PayloadAttributes) (eengine.ForkChoiceResponse, error) {
						return eengine.ForkChoiceResponse{
							PayloadStatus: eengine.PayloadStatusV1{
								Status:          eengine.INVALID,
								LatestValidHash: nil,
								ValidationError: nil,
							},
							PayloadID: nil,
						}, nil
					},
				},
				mockClient: mock.MockClient{},
				req: &abci.RequestPrepareProposal{
					Txs:    nil,
					Height: 2,
					Time:   time.Now(),
				},
				wantErr: true,
				setupMocks: func(esk *moduletestutil.MockEvmStakingKeeper) {
					esk.EXPECT().PeekEligibleWithdrawals(gomock.Any()).Return(nil, nil)
				},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				ctx, storeService := setupCtxStore(t, nil)
				cdc := getCodec(t)
				txConfig := authtx.NewTxConfig(cdc, nil)

				ctrl := gomock.NewController(t)
				ak := moduletestutil.NewMockAccountKeeper(ctrl)
				esk := moduletestutil.NewMockEvmStakingKeeper(ctrl)
				uk := moduletestutil.NewMockUpgradeKeeper(ctrl)

				if tt.setupMocks != nil {
					tt.setupMocks(esk)
				}

				var err error
				tt.mockEngine.EngineClient, err = ethclient.NewEngineMock()
				require.NoError(t, err)

				k, err := NewKeeper(cdc, storeService, &tt.mockEngine, &tt.mockClient, txConfig, ak, esk, uk)
				require.NoError(t, err)
				k.SetValidatorAddress(common.BytesToAddress([]byte("test")))
				populateGenesisHead(ctx, t, k)

				tt.req.MaxTxBytes = cmttypes.MaxBlockSizeBytes

				_, err = k.PrepareProposal(withRandomErrs(t, ctx), tt.req)
				if (err != nil) != tt.wantErr {
					t.Errorf("PrepareProposal() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
			})
		}
	})

	t.Run("TestBuildNonOptimistic", func(t *testing.T) {
		t.Parallel()
		// setup dependencies
		ctx, storeService := setupCtxStore(t, nil)
		cdc := getCodec(t)
		txConfig := authtx.NewTxConfig(cdc, nil)

		mockEngine, err := newMockEngineAPI(0)
		require.NoError(t, err)

		ctrl := gomock.NewController(t)
		mockClient := mock.NewMockClient(ctrl)
		ak := moduletestutil.NewMockAccountKeeper(ctrl)
		esk := moduletestutil.NewMockEvmStakingKeeper(ctrl)
		uk := moduletestutil.NewMockUpgradeKeeper(ctrl)

		// Expected call for PeekEligibleWithdrawals
		esk.EXPECT().PeekEligibleWithdrawals(gomock.Any()).Return(nil, nil).AnyTimes()

		keeper, err := NewKeeper(cdc, storeService, &mockEngine, mockClient, txConfig, ak, esk, uk)
		require.NoError(t, err)
		keeper.SetValidatorAddress(common.BytesToAddress([]byte("test")))
		populateGenesisHead(ctx, t, keeper)

		// get the genesis block to build on top of
		// Get the parent block we will build on top of
		head, err := keeper.getExecutionHead(ctx)
		require.NoError(t, err)

		req := &abci.RequestPrepareProposal{
			Txs:        nil,
			Height:     int64(2),
			Time:       time.Now(),
			MaxTxBytes: cmttypes.MaxBlockSizeBytes,
		}

		resp, err := keeper.PrepareProposal(withRandomErrs(t, ctx), req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		msgDelegate := stypes.NewMsgDelegate("delAddr", "valAddr", sdk.NewInt64Coin("stake", 100))
		resp.Txs[0] = appendMsgToTx(t, txConfig, resp.Txs[0], msgDelegate)

		// decode the txn and get the messages
		tx, err := txConfig.TxDecoder()(resp.Txs[0])
		require.NoError(t, err)

		// assert that the message is an executable payload
		for _, msg := range tx.GetMsgs() {
			if _, ok := msg.(*etypes.MsgExecutionPayload); ok {
				assertExecutablePayload(t, msg, req.Time.Unix(), head.Hash(), keeper.validatorAddr, head.GetBlockHeight()+1)
			}
		}
	})
}

// appendMsgToTx appends the given message to the unpacked transaction and returns the new packed transaction bytes.
func appendMsgToTx(t *testing.T, txConfig client.TxConfig, txBytes []byte, msg sdk.Msg) []byte {
	t.Helper()
	txn, err := txConfig.TxDecoder()(txBytes)
	require.NoError(t, err)

	b := txConfig.NewTxBuilder()
	err = b.SetMsgs(append(txn.GetMsgs(), msg)...)
	require.NoError(t, err)

	newTxBytes, err := txConfig.TxEncoder()(b.GetTx())
	require.NoError(t, err)

	return newTxBytes
}

// assertExecutablePayload asserts that the given message is an executable payload with the expected values.
func assertExecutablePayload(t *testing.T, msg sdk.Msg, ts int64, blockHash common.Hash, validatorAddr common.Address, height uint64) {
	t.Helper()
	executionPayload, ok := msg.(*etypes.MsgExecutionPayload)
	require.True(t, ok)
	require.NotNil(t, executionPayload)

	payload := new(eengine.ExecutableData)
	err := json.Unmarshal(executionPayload.GetExecutionPayload(), payload)
	require.NoError(t, err)
	require.Equal(t, int64(payload.Timestamp), ts)
	require.Equal(t, payload.Random, blockHash)
	require.Equal(t, payload.FeeRecipient, validatorAddr)
	require.Empty(t, payload.Withdrawals)
	require.Equal(t, payload.Number, height)

	// require.Len(t, executionPayload.PrevPayloadEvents, 1)
	// evmLog := executionPayload.PrevPayloadEvents[0]
	// require.Equal(t, evmLog.Address, zeroAddr.Bytes())
}

func ctxWithAppHash(t *testing.T, appHash common.Hash) context.Context {
	t.Helper()
	ctx, _ := setupCtxStore(t, &cmtproto.Header{AppHash: appHash.Bytes()})

	return ctx
}

func setupCtxStore(t *testing.T, header *cmtproto.Header) (sdk.Context, store.KVStoreService) {
	t.Helper()
	key := storetypes.NewKVStoreKey("test")
	storeService := runtime.NewKVStoreService(key)
	testCtx := testutil.DefaultContextWithDB(t, key, storetypes.NewTransientStoreKey("transient_test"))
	if header == nil {
		header = &cmtproto.Header{Time: cmttime.Now()}
	}
	ctx := testCtx.Ctx.WithBlockHeader(*header)

	return ctx, storeService
}

func getCodec(t *testing.T) codec.Codec {
	t.Helper()
	sdkConfig := sdk.GetConfig()
	reg, err := codectypes.NewInterfaceRegistryWithOptions(codectypes.InterfaceRegistryOptions{
		ProtoFiles: proto.HybridResolver,
		SigningOptions: signing.Options{
			AddressCodec:          authcodec.NewBech32Codec(sdkConfig.GetBech32AccountAddrPrefix()),
			ValidatorAddressCodec: authcodec.NewBech32Codec(sdkConfig.GetBech32ValidatorAddrPrefix()),
		},
	})
	require.NoError(t, err)

	cosmosstd.RegisterInterfaces(reg)
	atypes.RegisterInterfaces(reg)
	stypes.RegisterInterfaces(reg)
	btypes.RegisterInterfaces(reg)
	dtypes.RegisterInterfaces(reg)
	etypes.RegisterInterfaces(reg)

	return codec.NewProtoCodec(reg)
}

var _ ethclient.EngineClient = (*mockEngineAPI)(nil)
var _ etypes.EvmEventProcessor = (*mockLogProvider)(nil)
var _ etypes.VoteExtensionProvider = (*mockVEProvider)(nil)

type mockEngineAPI struct {
	ethclient.EngineClient
	syncings                <-chan struct{}
	fuzzer                  *fuzz.Fuzzer
	mock                    ethclient.EngineClient // avoid repeating the implementation but also allow for custom implementations of mocks
	headerByTypeFunc        func(context.Context, ethclient.HeadType) (*types.Header, error)
	forkchoiceUpdatedV3Func func(context.Context, eengine.ForkchoiceStateV1, *eengine.PayloadAttributes) (eengine.ForkChoiceResponse, error)
	newPayloadV3Func        func(context.Context, eengine.ExecutableData, []common.Hash, *common.Hash) (eengine.PayloadStatusV1, error)
}

// newMockEngineAPI returns a new mock engine API with a fuzzer and a mock engine client.
func newMockEngineAPI(syncings int) (mockEngineAPI, error) {
	me, err := ethclient.NewEngineMock()
	if err != nil {
		return mockEngineAPI{}, err
	}

	syncs := make(chan struct{}, syncings)
	for range syncings {
		syncs <- struct{}{}
	}

	return mockEngineAPI{
		mock:     me,
		syncings: syncs,
		fuzzer:   ethclient.NewFuzzer(time.Now().Truncate(time.Hour * 24).Unix()),
	}, nil
}

type mockVEProvider struct{}

func (m mockVEProvider) PrepareVotes(_ context.Context, _ abci.ExtendedCommitInfo) ([]sdk.Msg, error) {
	coin := sdk.NewInt64Coin("stake", 100)
	msg := stypes.NewMsgDelegate("addr", "addr", coin)

	return []sdk.Msg{msg}, nil
}

type mockLogProvider struct {
	deliverErr error
}

func (m mockLogProvider) Name() string {
	return "mock"
}

func (m mockLogProvider) Prepare(_ context.Context, blockHash common.Hash) ([]*etypes.EVMEvent, error) {
	f := fuzz.NewWithSeed(int64(blockHash[0]))

	var topic common.Hash
	f.Fuzz(&topic)

	return []*etypes.EVMEvent{{
		Address: zeroAddr.Bytes(),
		Topics:  [][]byte{topic[:]},
	}}, nil
}

func (m mockLogProvider) Addresses() []common.Address {
	return []common.Address{zeroAddr}
}

func (m mockLogProvider) Deliver(_ context.Context, _ common.Hash, log *etypes.EVMEvent) error {
	if !bytes.Equal(log.Address, zeroAddr.Bytes()) {
		panic("unexpected evm log address")
	}

	return m.deliverErr
}

func (m mockEngineAPI) maybeSync() (eengine.PayloadStatusV1, bool) {
	select {
	case <-m.syncings:
		return eengine.PayloadStatusV1{
			Status: eengine.SYNCING,
		}, true
	default:
		return eengine.PayloadStatusV1{}, false
	}
}

func (mockEngineAPI) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return nil, nil
}

func (m *mockEngineAPI) HeaderByType(ctx context.Context, typ ethclient.HeadType) (*types.Header, error) {
	if m.headerByTypeFunc != nil {
		return m.headerByTypeFunc(ctx, typ)
	}

	return m.mock.HeaderByType(ctx, typ)
}

func (m *mockEngineAPI) NewPayloadV2(ctx context.Context, params eengine.ExecutableData) (eengine.PayloadStatusV1, error) {
	return m.mock.NewPayloadV2(ctx, params)
}

//nolint:nonamedreturns // Required for defer
func (m *mockEngineAPI) NewPayloadV3(ctx context.Context, params eengine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash) (resp eengine.PayloadStatusV1, err error) {
	if status, ok := m.maybeSync(); ok {
		defer func() {
			resp.Status = status.Status
		}()
	}

	if m.newPayloadV3Func != nil {
		return m.newPayloadV3Func(ctx, params, versionedHashes, beaconRoot)
	}

	return m.mock.NewPayloadV3(ctx, params, versionedHashes, beaconRoot)
}

//nolint:nonamedreturns // Required for defer
func (m *mockEngineAPI) ForkchoiceUpdatedV3(ctx context.Context, update eengine.ForkchoiceStateV1, payloadAttributes *eengine.PayloadAttributes) (resp eengine.ForkChoiceResponse, err error) {
	if status, ok := m.maybeSync(); ok {
		defer func() {
			resp.PayloadStatus.Status = status.Status
		}()
	}

	if m.forkchoiceUpdatedV3Func != nil {
		return m.forkchoiceUpdatedV3Func(ctx, update, payloadAttributes)
	}

	return m.mock.ForkchoiceUpdatedV3(ctx, update, payloadAttributes)
}

func (m *mockEngineAPI) GetPayloadV3(ctx context.Context, payloadID eengine.PayloadID) (*eengine.ExecutionPayloadEnvelope, error) {
	return m.mock.GetPayloadV3(ctx, payloadID)
}

// nextBlock creates a new block with the given height, timestamp, parentHash, and feeRecipient. It also returns the
// payload for the block. It's a utility function for testing.
func (m *mockEngineAPI) nextBlock(
	t *testing.T,
	height uint64,
	timestamp uint64,
	parentHash common.Hash,
	feeRecipient common.Address,
	beaconRoot *common.Hash,
) (*types.Block, eengine.ExecutableData) {
	t.Helper()
	var header types.Header
	m.fuzzer.Fuzz(&header)
	header.Number = big.NewInt(int64(height))
	header.Time = timestamp
	header.ParentHash = parentHash
	header.Coinbase = feeRecipient
	header.MixDigest = parentHash
	header.ParentBeaconRoot = beaconRoot

	// Convert header to block
	block := types.NewBlock(&header, nil, nil, trie.NewStackTrie(nil))

	// Convert block to payload
	env := eengine.BlockToExecutableData(block, big.NewInt(0), nil)
	payload := *env.ExecutionPayload

	// Ensure the block is valid
	_, err := eengine.ExecutableDataToBlock(payload, nil, beaconRoot)
	require.NoError(t, err)

	return block, payload
}

func withRandomErrs(t *testing.T, ctx sdk.Context) sdk.Context {
	t.Helper()
	return ctx.WithContext(ethclient.WithRandomErr(ctx, t))
}