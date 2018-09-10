package pub

import (
	orderPkg "github.com/BiJie/BinanceChain/plugins/dex/order"
)

// intermediate data structures to deal with concurrent publication between main thread and publisher thread
type BlockInfoToPublish struct {
	height             int64
	timestamp          int64
	tradesToPublish    []Trade
	orderChanges       orderPkg.OrderChanges
	orderChangesMap    orderPkg.OrderChangesMap
	accounts           map[string]Account
	latestPricesLevels orderPkg.ChangedPriceLevels
}

func NewBlockInfoToPublish(
	height int64,
	timestamp int64,
	tradesToPublish []Trade,
	orderChanges orderPkg.OrderChanges,
	orderChangesMap orderPkg.OrderChangesMap,
	accounts map[string]Account,
	latestPriceLevels orderPkg.ChangedPriceLevels) BlockInfoToPublish {
	return BlockInfoToPublish{
		height,
		timestamp,
		tradesToPublish,
		orderChanges,
		orderChangesMap,
		accounts,
		latestPriceLevels}
}
