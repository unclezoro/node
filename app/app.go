package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	abci "github.com/tendermint/tendermint/abci/types"
	cmn "github.com/tendermint/tendermint/libs/common"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"

	"github.com/BiJie/BinanceChain/app/config"
	"github.com/BiJie/BinanceChain/app/pub"
	"github.com/BiJie/BinanceChain/common"
	bnclog "github.com/BiJie/BinanceChain/common/log"
	"github.com/BiJie/BinanceChain/common/tx"
	"github.com/BiJie/BinanceChain/common/types"
	"github.com/BiJie/BinanceChain/common/utils"
	"github.com/BiJie/BinanceChain/plugins/dex"
	"github.com/BiJie/BinanceChain/plugins/dex/matcheng"
	"github.com/BiJie/BinanceChain/plugins/dex/order"
	"github.com/BiJie/BinanceChain/plugins/ico"
	"github.com/BiJie/BinanceChain/plugins/tokens"
	tokenStore "github.com/BiJie/BinanceChain/plugins/tokens/store"
	"github.com/BiJie/BinanceChain/wire"
)

const (
	appName = "BNBChain"
)

const (
	DefaultLogFile     = "bnc.log"
	DefaultLogBuffSize = 10000
)

// default home directories for expected binaries
var (
	DefaultCLIHome  = os.ExpandEnv("$HOME/.bnbcli")
	DefaultNodeHome = os.ExpandEnv("$HOME/.bnbchaind")
)

// BinanceChain implements ChainApp
var _ types.ChainApp = (*BinanceChain)(nil)

var (
	Codec         = MakeCodec()
	ServerContext = config.NewDefaultContext()
)

// BinanceChain is the BNBChain ABCI application
type BinanceChain struct {
	*BaseApp
	Codec *wire.Codec

	// the abci query handler mapping is `prefix -> handler`
	queryHandlers map[string]types.AbciQueryHandler

	// keepers
	FeeCollectionKeeper tx.FeeCollectionKeeper
	CoinKeeper          bank.Keeper
	DexKeeper           *dex.DexKeeper
	AccountMapper       auth.AccountMapper
	TokenMapper         tokenStore.Mapper

	publicationConfig *config.PublicationConfig
	publisher         pub.MarketDataPublisher
}

// NewBinanceChain creates a new instance of the BinanceChain.
func NewBinanceChain(logger log.Logger, db dbm.DB, traceStore io.Writer, baseAppOptions ...func(*BaseApp)) *BinanceChain {

	// create app-level codec for txs and accounts
	var cdc = Codec

	// create composed tx decoder
	decoders := wire.ComposeTxDecoders(cdc, defaultTxDecoder)

	// create your application object
	var app = &BinanceChain{
		BaseApp:           NewBaseApp(appName, cdc, logger, db, decoders, ServerContext.PublishAccountBalance, baseAppOptions...),
		Codec:             cdc,
		queryHandlers:     make(map[string]types.AbciQueryHandler),
		publicationConfig: ServerContext.PublicationConfig,
	}

	app.SetCommitMultiStoreTracer(traceStore)
	// mappers
	app.AccountMapper = auth.NewAccountMapper(cdc, common.AccountStoreKey, types.ProtoAppAccount)
	app.TokenMapper = tokenStore.NewMapper(cdc, common.TokenStoreKey)

	// Add handlers.
	app.CoinKeeper = bank.NewKeeper(app.AccountMapper)
	// TODO: make the concurrency configurable

	tradingPairMapper := dex.NewTradingPairMapper(cdc, common.PairStoreKey)
	app.DexKeeper = dex.NewOrderKeeper(common.DexStoreKey, app.CoinKeeper, tradingPairMapper,
		app.RegisterCodespace(dex.DefaultCodespace), 2, app.cdc, app.publicationConfig.PublishMarketData)
	// Currently we do not need the ibc and staking part
	// app.ibcMapper = ibc.NewMapper(app.cdc, app.capKeyIBCStore, app.RegisterCodespace(ibc.DefaultCodespace))
	// app.stakeKeeper = simplestake.NewKeeper(app.capKeyStakingStore, app.coinKeeper, app.RegisterCodespace(simplestake.DefaultCodespace))

	app.registerHandlers(cdc)

	if app.publicationConfig.PublishMarketData ||
		app.publicationConfig.PublishAccountBalance ||
		app.publicationConfig.PublishOrderBook {
		app.publisher = pub.MarketDataPublisher{
			Logger:            app.Logger.With("module", "pub"),
			ToPublishCh:       make(chan pub.BlockInfoToPublish, pub.PublicationChannelSize),
			ToRemoveOrderIdCh: make(chan string, pub.ToRemoveOrderIdChannelSize),
			RemoveDoneCh:      make(chan struct{}),
		}
		if err := app.publisher.Init(app.publicationConfig); err != nil {
			app.publisher.Stop()
			app.Logger.Error("Cannot start up market data kafka publisher", "err", err)
			/**
			  TODO(#66): we should return nil here, but cosmos start-up logic doesn't process nil newapp vendor/github.com/cosmos/cosmos-sdk/server/constructors.go:34
			  app := appFn(logger, db, traceStoreWriter)
			  return app, nil
			*/
		}
	}

	// Initialize BaseApp.
	app.SetInitChainer(app.initChainerFn())
	app.SetEndBlocker(app.EndBlocker)
	app.MountStoresIAVL(common.MainStoreKey, common.AccountStoreKey, common.TokenStoreKey, common.DexStoreKey, common.PairStoreKey)
	app.SetAnteHandler(tx.NewAnteHandler(app.AccountMapper, app.FeeCollectionKeeper))
	err := app.LoadLatestVersion(common.MainStoreKey)
	if err != nil {
		cmn.Exit(err.Error())
	}

	app.initPlugins()
	return app
}

func (app *BinanceChain) initPlugins() {
	if app.checkState == nil {
		return
	}

	tokens.InitPlugin(app, app.TokenMapper)
	dex.InitPlugin(app, app.DexKeeper)

	app.DexKeeper.FeeConfig.Init(app.checkState.ctx)
	// count back to 7 days.
	app.DexKeeper.InitOrderBook(app.checkState.ctx, 7, loadBlockDB(), app.LastBlockHeight(), app.txDecoder)
}

// Query performs an abci query.
func (app *BinanceChain) Query(req abci.RequestQuery) (res abci.ResponseQuery) {
	path := splitPath(req.Path)
	if len(path) == 0 {
		msg := "no query path provided"
		return sdk.ErrUnknownRequest(msg).QueryResult()
	}
	prefix := path[0]
	if handler, ok := app.queryHandlers[prefix]; ok {
		res := handler(app, req, path)
		if res == nil {
			return app.BaseApp.Query(req)
		}
		return *res
	}
	return app.BaseApp.Query(req)
}

func (app *BinanceChain) registerHandlers(cdc *wire.Codec) {
	sdkBankHandler := bank.NewHandler(app.CoinKeeper)
	bankHandler := func(ctx sdk.Context, msg sdk.Msg, simulate bool) sdk.Result {
		return sdkBankHandler(ctx, msg)
	}
	app.Router().AddRoute("bank", bankHandler)
	for route, handler := range tokens.Routes(app.TokenMapper, app.AccountMapper, app.CoinKeeper) {
		app.Router().AddRoute(route, handler)
	}
	for route, handler := range dex.Routes(cdc, app.DexKeeper, app.TokenMapper, app.AccountMapper) {
		app.Router().AddRoute(route, handler)
	}
}

// RegisterQueryHandler registers an abci query handler.
func (app *BinanceChain) RegisterQueryHandler(prefix string, handler types.AbciQueryHandler) {
	if _, ok := app.queryHandlers[prefix]; ok {
		panic(fmt.Errorf("registerQueryHandler: prefix `%s` is already registered", prefix))
	} else {
		app.queryHandlers[prefix] = handler
	}
}

// initChainerFn performs custom logic for chain initialization.
func (app *BinanceChain) initChainerFn() sdk.InitChainer {
	return func(ctx sdk.Context, req abci.RequestInitChain) abci.ResponseInitChain {
		stateJSON := req.AppStateBytes

		genesisState := new(GenesisState)
		err := app.Codec.UnmarshalJSON(stateJSON, genesisState)
		if err != nil {
			panic(err) // TODO https://github.com/cosmos/cosmos-sdk/issues/468
			// return sdk.ErrGenesisParse("").TraceCause(err, "")
		}

		for _, gacc := range genesisState.Accounts {
			acc := gacc.ToAppAccount()
			acc.AccountNumber = app.AccountMapper.GetNextAccountNumber(ctx)
			app.AccountMapper.SetAccount(ctx, acc)
		}

		for _, token := range genesisState.Tokens {
			// TODO: replace by Issue and move to token.genesis
			err = app.TokenMapper.NewToken(ctx, token)
			if err != nil {
				panic(err)
			}

			_, _, sdkErr := app.CoinKeeper.AddCoins(ctx, token.Owner, append((sdk.Coins)(nil),
				sdk.Coin{
					Denom:  token.Symbol,
					Amount: sdk.NewInt(token.TotalSupply.ToInt64()),
				}))
			if sdkErr != nil {
				panic(sdkErr)
			}
		}

		// Application specific genesis handling
		app.DexKeeper.InitGenesis(ctx, genesisState.DexGenesis.TradingGenesis)
		return abci.ResponseInitChain{}
	}
}

func (app *BinanceChain) EndBlocker(ctx sdk.Context, req abci.RequestEndBlock) abci.ResponseEndBlock {
	// lastBlockTime would be 0 if this is the first block.
	lastBlockTime := app.checkState.ctx.BlockHeader().Time
	blockTime := ctx.BlockHeader().Time
	height := ctx.BlockHeight()

	var tradesToPublish []pub.Trade

	if utils.SameDayInUTC(lastBlockTime, blockTime) || height == 1 {
		// only match in the normal block
		app.Logger.Debug(fmt.Sprintf("normal block: %d", height))
		if app.publicationConfig.PublishMarketData && app.publisher.IsLive {
			// group trades by Bid and Sid to make fee update easier
			groupedTrades := make(map[string]map[string]*pub.Trade)

			transCh := make(chan order.Transfer, pub.FeeCollectionChannelSize)

			var feeCollectorForTrades = func(trans order.Transfer) {
				transCh <- trans
			}

			ctx, _, _ = app.DexKeeper.MatchAndAllocateAll(ctx, app.AccountMapper, feeCollectorForTrades)
			close(transCh)

			for tran := range transCh {
				app.Logger.Debug(fmt.Sprintf("fee Collector for trans: %s", tran.String()))
				if tran.IsExpired() {
					if !tran.FeeFree() {
						// we must only have ioc expire here
						if tran.IsBuyer() {
							app.DexKeeper.OrderChangesMap[tran.Bid].FeeAsset = tran.Fee.Tokens[0].Denom
							app.DexKeeper.OrderChangesMap[tran.Bid].Fee = tran.Fee.Tokens[0].Amount.Int64()
						} else {
							app.DexKeeper.OrderChangesMap[tran.Sid].FeeAsset = tran.Fee.Tokens[0].Denom
							app.DexKeeper.OrderChangesMap[tran.Sid].Fee = tran.Fee.Tokens[0].Amount.Int64()
						}
					}
				} else {
					// for partial and fully filled order fee
					var t *pub.Trade
					if groupedByBid, exists := groupedTrades[tran.Bid]; exists {
						if tradeToPublish, exists := groupedByBid[tran.Sid]; exists {
							t = tradeToPublish
						} else {
							t = new(pub.Trade)
							t.Sid = tran.Sid
							t.Bid = tran.Bid
							groupedByBid[tran.Sid] = t
						}
					} else {
						groupedByBid := make(map[string]*pub.Trade)
						groupedTrades[tran.Bid] = groupedByBid
						t = new(pub.Trade)
						t.Sid = tran.Sid
						t.Bid = tran.Bid
						groupedByBid[tran.Sid] = t
					}

					// TODO(#66): Fix potential fee precision loss
					if !tran.FeeFree() {
						if tran.IsBuyer() {
							t.Bfee = tran.Fee.Tokens[0].Amount.Int64()
							t.BfeeAsset = tran.Fee.Tokens[0].Denom
						} else {
							t.Sfee = tran.Fee.Tokens[0].Amount.Int64()
							t.SfeeAsset = tran.Fee.Tokens[0].Denom
						}
					}
				}
			}

			tradeIdx := 0
			var allTrades *map[string][]matcheng.Trade
			allTrades = app.DexKeeper.GetLastTrades()
			for symbol, trades := range *allTrades {
				for _, trade := range trades {
					app.Logger.Debug(fmt.Sprintf("processing trade: %s-%s", trade.BId, trade.SId))
					if groupedByBid, exists := groupedTrades[trade.BId]; exists {
						if t, exists := groupedByBid[trade.SId]; exists {
							t.Id = fmt.Sprintf("%d-%d", ctx.BlockHeader().Height, tradeIdx)
							t.Symbol = symbol
							t.Price = trade.LastPx
							t.Qty = trade.LastQty
							t.BuyCumQty = trade.BuyCumQty
							tradesToPublish = append(tradesToPublish, *t)
							tradeIdx += 1
						} else {
							app.Logger.Error(fmt.Sprintf("failed to look up sid from trade: %s-%s from groupedTrades", trade.BId, trade.SId))
						}
					} else {
						app.Logger.Error(fmt.Sprintf("failed to look up bid from trade: %s-%s from groupedTrades", trade.BId, trade.SId))
					}
				}
			}
		} else {
			ctx, _, _ = app.DexKeeper.MatchAndAllocateAll(ctx, app.AccountMapper, nil)
		}
	} else {
		// breathe block
		bnclog.Info("Start Breathe Block Handling",
			"height", height, "lastBlockTime", lastBlockTime, "newBlockTime", blockTime)
		icoDone := ico.EndBlockAsync(ctx)
		dex.EndBreatheBlock(ctx, app.AccountMapper, app.DexKeeper, height, blockTime)

		// other end blockers
		<-icoDone
	}

	// distribute fees TODO: enable it after upgraded to tm 0.24.0
	// distributeFee(ctx, app.AccountMapper)
	// TODO: update validators

	if app.publisher.ShouldPublish() {
		app.Logger.Info(fmt.Sprintf("start to collect publish information at height: %d", height))

		txRelatedAccounts, hasTxRelatedAccountsChanges := ctx.Value(InvolvedAddressKey).(map[string]bool)
		// TODO(#66): confirm the performance is acceptable when there are a lot of orders and books here (orders might get accumulated for 3 days - the time limit of GTC order to expire)
		orders, ordersMap := app.DexKeeper.GetLastOrdersCopy()
		var tradeRelatedAccounts *map[string]bool
		var accountsToPublish map[string]pub.Account
		if app.publicationConfig.PublishAccountBalance {
			tradeRelatedAccounts = app.DexKeeper.GetTradeRelatedAccounts(orders)
			if hasTxRelatedAccountsChanges {
				accountsToPublish = app.getAllChangedAccountBalances(txRelatedAccounts, *tradeRelatedAccounts)
			} else {
				accountsToPublish = app.getAllChangedAccountBalances(map[string]bool{}, *tradeRelatedAccounts)
			}
		}
		var latestPriceLevels order.ChangedPriceLevels
		if app.publicationConfig.PublishOrderBook {
			latestPriceLevels = app.DexKeeper.GetOrderBookForPublish(20)
		}
		app.Logger.Info(fmt.Sprintf(
			"start to publish at block: %d, blockTime: %d, numOfTrades: %d, partial order changes: %d",
			ctx.BlockHeader().Height,
			blockTime,
			len(tradesToPublish),
			len(orders)))
		app.publisher.ToPublishCh <- pub.NewBlockInfoToPublish(ctx.BlockHeader().Height, blockTime, tradesToPublish, orders, ordersMap, accountsToPublish, latestPriceLevels)

		// clean up intermediate cached data
		app.DexKeeper.ClearOrderChanges()
		app.deliverState.ctx = ctx.WithValue(InvolvedAddressKey, map[string]bool{})

		// remove item from OrderChangesMap when we published removed order (cancel, iocnofill, fullyfilled, expired)
	cont:
		for {
			select {
			case id := <-app.publisher.ToRemoveOrderIdCh:
				app.Logger.Debug(fmt.Sprintf("delete order %s from order changes map", id))
				delete(app.DexKeeper.OrderChangesMap, id)
			case <-app.publisher.RemoveDoneCh:
				app.Logger.Info(fmt.Sprintf("done remove orders from order changes map"))
				break cont
			}
		}
	}

	return abci.ResponseEndBlock{}
}

// ExportAppStateAndValidators exports blockchain world state to json.
func (app *BinanceChain) ExportAppStateAndValidators() (appState json.RawMessage, validators []tmtypes.GenesisValidator, err error) {
	ctx := app.NewContext(true, abci.Header{})

	// iterate to get the accounts
	accounts := []GenesisAccount{}
	appendAccount := func(acc auth.Account) (stop bool) {
		account := GenesisAccount{
			Address: acc.GetAddress(),
		}
		accounts = append(accounts, account)
		return false
	}
	app.AccountMapper.IterateAccounts(ctx, appendAccount)

	genState := GenesisState{
		Accounts: accounts,
	}
	appState, err = wire.MarshalJSONIndent(app.cdc, genState)
	if err != nil {
		return nil, nil, err
	}
	return appState, validators, nil
}

// GetCodec returns the app's Codec.
func (app *BinanceChain) GetCodec() *wire.Codec {
	return app.Codec
}

// GetContextForCheckState gets the context for the check state.
func (app *BinanceChain) GetContextForCheckState() sdk.Context {
	return app.checkState.ctx
}

// default custom logic for transaction decoding
func defaultTxDecoder(cdc *wire.Codec) sdk.TxDecoder {
	return func(txBytes []byte) (sdk.Tx, sdk.Error) {
		var tx = auth.StdTx{}

		if len(txBytes) == 0 {
			return nil, sdk.ErrTxDecode("txBytes are empty")
		}

		// StdTx.Msg is an interface. The concrete types
		// are registered by MakeTxCodec
		err := cdc.UnmarshalBinary(txBytes, &tx)
		if err != nil {
			return nil, sdk.ErrTxDecode("").TraceSDK(err.Error())
		}
		return tx, nil
	}
}

// MakeCodec creates a custom tx codec.
func MakeCodec() *wire.Codec {
	var cdc = wire.NewCodec()

	wire.RegisterCrypto(cdc) // Register crypto.
	bank.RegisterWire(cdc)
	sdk.RegisterWire(cdc) // Register Msgs
	dex.RegisterWire(cdc)
	tokens.RegisterWire(cdc)
	types.RegisterWire(cdc)
	tx.RegisterWire(cdc)

	return cdc
}

func (app *BinanceChain) getAllChangedAccountBalances(txRelatedAccounts map[string]bool, tradeRelatedAccounts map[string]bool) map[string]pub.Account {
	res := make(map[string]pub.Account)

	app.getAccountBalances(res, txRelatedAccounts)
	app.getAccountBalances(res, tradeRelatedAccounts)

	return res
}

func (app *BinanceChain) getAccountBalances(res map[string]pub.Account, accs map[string]bool) {
	for bech32Str, _ := range accs {
		if _, ok := res[bech32Str]; !ok {
			addr, _ := sdk.AccAddressFromBech32(bech32Str)
			if acc, ok := app.AccountMapper.GetAccount(app.deliverState.ctx, addr).(types.NamedAccount); ok {
				assetsMap := make(map[string]*pub.AssetBalance)
				// TODO(#66): set the length to be the total coins this account owned
				assets := make([]pub.AssetBalance, 0, 10)

				for _, freeCoin := range acc.GetCoins() {
					if assetBalance, ok := assetsMap[freeCoin.Denom]; ok {
						assetBalance.Free = freeCoin.Amount.Int64()
					} else {
						newAB := pub.AssetBalance{Asset: freeCoin.Denom, Free: freeCoin.Amount.Int64()}
						assets = append(assets, newAB)
						assetsMap[freeCoin.Denom] = &newAB
					}
				}

				for _, frozenCoin := range acc.GetFrozenCoins() {
					if assetBalance, ok := assetsMap[frozenCoin.Denom]; ok {
						assetBalance.Frozen = frozenCoin.Amount.Int64()
					} else {
						assetsMap[frozenCoin.Denom] = &pub.AssetBalance{Asset: frozenCoin.Denom, Frozen: frozenCoin.Amount.Int64()}
					}
				}

				for _, lockedCoin := range acc.GetLockedCoins() {
					if assetBalance, ok := assetsMap[lockedCoin.Denom]; ok {
						assetBalance.Locked = lockedCoin.Amount.Int64()
					} else {
						assetsMap[lockedCoin.Denom] = &pub.AssetBalance{Asset: lockedCoin.Denom, Locked: lockedCoin.Amount.Int64()}
					}
				}

				res[bech32Str] = pub.Account{bech32Str, assets}
			} else {
				app.Logger.Error(fmt.Sprintf("failed to get account %s from AccountMapper", bech32Str))
			}
		}
	}
}
