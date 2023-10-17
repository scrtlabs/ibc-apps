package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cosmossdk.io/math"
	"github.com/strangelove-ventures/interchaintest/v7"
	"github.com/strangelove-ventures/interchaintest/v7/chain/cosmos"
	"github.com/strangelove-ventures/interchaintest/v7/ibc"
	interchaintestrelayer "github.com/strangelove-ventures/interchaintest/v7/relayer"
	"github.com/strangelove-ventures/interchaintest/v7/testreporter"
	"github.com/strangelove-ventures/interchaintest/v7/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// TestIBCHooks ensures the ibc-hooks middleware from osmosis works as expected.
func TestIBCHooks(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	t.Parallel()

	// Create chain factory with osmosis and osmosis2
	numVals := 1
	numFullNodes := 0

	genesisWalletAmount := int64(10_000_000)

	cfg := ibc.ChainConfig{
		Name:    "osmosis",
		Type:    "cosmos",
		ChainID: "simapp-1",
		Bin:     "simd",
		Images: []ibc.DockerImage{
			{
				Repository: "ibchooks",
				Version:    "local",
				UidGid:     "1025:1025",
			},
		},
		Bech32Prefix: "cosmos",
		Denom:        "uosmo",
		CoinType:     "118",
	}

	cfg2 := cfg.Clone()
	cfg2.Name = "osmosis-counterparty"
	cfg2.ChainID = "counterparty-2"

	cf := interchaintest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*interchaintest.ChainSpec{
		{
			Name:          "osmosis",
			ChainConfig:   cfg,
			NumValidators: &numVals,
			NumFullNodes:  &numFullNodes,
		},
		{
			Name:          "osmosis",
			ChainConfig:   cfg2,
			NumValidators: &numVals,
			NumFullNodes:  &numFullNodes,
		},
	})

	const (
		path = "ibc-path"
	)

	// Get chains from the chain factory
	chains, err := cf.Chains(t.Name())
	require.NoError(t, err)

	client, network := interchaintest.DockerSetup(t)

	osmosis, osmosis2 := chains[0].(*cosmos.CosmosChain), chains[1].(*cosmos.CosmosChain)

	relayerType, relayerName := ibc.CosmosRly, "relay"

	// Get a relayer instance
	rf := interchaintest.NewBuiltinRelayerFactory(
		relayerType,
		zaptest.NewLogger(t),
		interchaintestrelayer.StartupFlags("--processor", "events", "--block-history", "100"),
	)

	r := rf.Build(t, client, network)

	ic := interchaintest.NewInterchain().
		AddChain(osmosis).
		AddChain(osmosis2).
		AddRelayer(r, relayerName).
		AddLink(interchaintest.InterchainLink{
			Chain1:  osmosis,
			Chain2:  osmosis2,
			Relayer: r,
			Path:    path,
		})

	ctx := context.Background()

	rep := testreporter.NewNopReporter()
	eRep := rep.RelayerExecReporter(t)

	require.NoError(t, ic.Build(ctx, eRep, interchaintest.InterchainBuildOptions{
		TestName:          t.Name(),
		Client:            client,
		NetworkID:         network,
		BlockDatabaseFile: interchaintest.DefaultBlockDatabaseFilepath(),
		SkipPathCreation:  false,
	}))
	t.Cleanup(func() {
		_ = ic.Close()
	})

	// Create some user accounts on both chains
	users := interchaintest.GetAndFundTestUsers(t, ctx, t.Name(), genesisWalletAmount, osmosis, osmosis2)

	err = r.StartRelayer(ctx, eRep, path)
	require.NoError(t, err)

	// Wait a few blocks for relayer to start and for user accounts to be created
	err = testutil.WaitForBlocks(ctx, 5, osmosis, osmosis2)
	require.NoError(t, err)

	// Get our Bech32 encoded user addresses
	osmosisUser, osmosis2User := users[0], users[1]

	osmosisUserAddr := osmosisUser.FormattedAddress()
	// osmosis2UserAddr := osmosis2User.FormattedAddress()

	channel, err := ibc.GetTransferChannel(ctx, r, eRep, osmosis.Config().ChainID, osmosis2.Config().ChainID)
	require.NoError(t, err)

	t.Cleanup(
		func() {
			err := r.StopRelayer(ctx, eRep)
			if err != nil {
				t.Logf("an error occurred while stopping the relayer: %s", err)
			}
		},
	)

	_, contractAddr := SetupContract(t, ctx, osmosis2, osmosis2User.KeyName(), "contracts/ibchooks_counter.wasm", `{"count":0}`)

	// do an ibc transfer through the memo to the other chain.
	transfer := ibc.WalletAmount{
		Address: contractAddr,
		Denom:   osmosis.Config().Denom,
		Amount:  math.NewInt(1),
	}

	memo := ibc.TransferOptions{
		Memo: fmt.Sprintf(`{"wasm":{"contract":"%s","msg":%s}}`, contractAddr, `{"increment":{}}`),
	}

	// Initial transfer. Account is created by the wasm execute is not so we must do this twice to properly set up
	transferTx, err := osmosis.SendIBCTransfer(ctx, channel.ChannelID, osmosisUser.KeyName(), transfer, memo)
	require.NoError(t, err)
	osmosisHeight, err := osmosis.Height(ctx)
	require.NoError(t, err)

	_, err = testutil.PollForAck(ctx, osmosis, osmosisHeight-5, osmosisHeight+25, transferTx.Packet)
	require.NoError(t, err)

	// Second time, this will make the counter == 1 since the account is now created.
	transferTx, err = osmosis.SendIBCTransfer(ctx, channel.ChannelID, osmosisUser.KeyName(), transfer, memo)
	require.NoError(t, err)
	osmosisHeight, err = osmosis.Height(ctx)
	require.NoError(t, err)

	_, err = testutil.PollForAck(ctx, osmosis, osmosisHeight-5, osmosisHeight+25, transferTx.Packet)
	require.NoError(t, err)

	// Get the address on the other chain's side
	addr := GetIBCHooksUserAddress(t, ctx, osmosis, channel.ChannelID, osmosisUserAddr)
	require.NotEmpty(t, addr)

	// Get funds on the receiving chain
	funds := GetIBCHookTotalFunds(t, ctx, osmosis2, contractAddr, addr)
	require.Equal(t, int(1), len(funds.Data.TotalFunds))

	var ibcDenom string
	for _, coin := range funds.Data.TotalFunds {
		if strings.HasPrefix(coin.Denom, "ibc/") {
			ibcDenom = coin.Denom
			break
		}
	}
	require.NotEmpty(t, ibcDenom)

	// ensure the count also increased to 1 as expected.
	count := GetIBCHookCount(t, ctx, osmosis2, contractAddr, addr)
	require.Equal(t, int64(1), count.Data.Count)
}