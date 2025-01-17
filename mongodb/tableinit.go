package mongodb

import (
	"gopkg.in/mgo.v2"
)

var (
	collectionSwap        *mgo.Collection
	collectionSwapPending *mgo.Collection
	collectionSwapDeleted *mgo.Collection
	collectionSyncedBlock *mgo.Collection
)

// do this when reconnect to the database
func deinintCollections() {
	collectionSwap = database.C(tbSwap)
	collectionSwapPending = database.C(tbSwapPending)
	collectionSwapDeleted = database.C(tbSwapDeleted)
	collectionSyncedBlock = database.C(tbSyncedBlock)
}

func initCollections() {
	initCollection(tbSwap, &collectionSwap, "txid")
	initCollection(tbSwapPending, &collectionSwapPending, "txid")
	initCollection(tbSwapDeleted, &collectionSwapDeleted, "txid")
	initCollection(tbSyncedBlock, &collectionSyncedBlock, "chain")
}

func initCollection(table string, collection **mgo.Collection, indexKey ...string) {
	*collection = database.C(table)
	if len(indexKey) != 0 && indexKey[0] != "" {
		_ = (*collection).EnsureIndexKey(indexKey...)
	}
}
