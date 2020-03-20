// Package db implements bridge to persistent storage represented by Mongo database.
package db

import (
	"context"
	"fantom-api-graphql/internal/types"
	"fmt"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	// coTransaction is the name of the off-chain database collection storing transaction details.
	coTransactions = "transaction"

	// fiTransactionPk is the name of the primary key field of the transaction collection.
	fiTransactionPk = "_id"

	// fiTransactionOrdinalIndex is the name of the transaction ordinal index in the blockchain field.
	fiTransactionOrdinalIndex = "orx"

	// fiTransactionBlock is the name of the block number field of the transaction.
	fiTransactionBlock = "blk"

	// fiTransactionSender is the name of the address field of the sender's account.
	fiTransactionSender = "from"

	// fiTransactionRecipient is the name of the address field of the recipients's account. null for contract creation.
	fiTransactionRecipient = "to"

	// fiTransactionValue is the name of the value transferred in WEI field.
	fiTransactionValue = "val"

	// fiTransactionTimestamp is the name of the transaction time stamp field.
	fiTransactionTimestamp = "ts"
)

/*
// tblTransaction represents a single transaction record in the database.
type tblTransaction struct {
	Id        common.Hash    `bson:"_id"`
	Block     uint64         `bson:"blk"`
	From      common.Address `bson:"from"`
	To        common.Address `bson:"to"`
	Value     hexutil.Big    `bson:"val"`
	Timestamp hexutil.Uint64 `bson:"tx"`
}
*/

// AddTransaction stores a transaction reference in connected persistent storage.
func (db *MongoDbBridge) AddTransaction(block *types.Block, trx *types.Transaction) error {
	// do we have all needed data?
	if block == nil || trx == nil {
		return fmt.Errorf("can not add empty transaction")
	}

	// check if the transaction already exists
	exists, err := db.isTransactionKnown(&trx.Hash)
	if err != nil {
		db.log.Critical(err)
		return err
	}

	// if the transaction already exists, we don't need to do anything here
	if exists {
		return nil
	}

	// get the collection for transactions
	col := db.client.Database(offChainDatabaseName).Collection(coTransactions)

	// recipient address may not be defined so we need to do a bit more parsing
	var rcAddress *string
	if trx.To != nil {
		rcp := trx.To.String()
		rcAddress = &rcp
	}

	// try to do the insert
	_, err = col.InsertOne(context.Background(), bson.D{
		{fiTransactionPk, trx.Hash.String()},
		{fiTransactionOrdinalIndex, trxOrdinalIndex(trx)},
		{fiTransactionBlock, uint64(block.Number)},
		{fiTransactionSender, trx.From.String()},
		{fiTransactionRecipient, rcAddress},
		{fiTransactionValue, trx.Value.String()},
		{fiTransactionTimestamp, uint64(block.TimeStamp)},
	})
	if err != nil {
		db.log.Critical(err)
		return err
	}

	// add the transaction to the sender's address list
	return db.propagateTrxToAccounts(block, trx)
}

// getTrxOrdinalIndex calculates ordinal index in the whole blockchain.
func trxOrdinalIndex(trx *types.Transaction) uint64 {
	return (uint64(*trx.BlockNumber) << 14) | uint64(*trx.TrxIndex)
}

// propagateTrxToAccounts push given transaction to sender's account and also to recipient's account, if exists.
func (db *MongoDbBridge) propagateTrxToAccounts(block *types.Block, trx *types.Transaction) error {
	// propagate to sender
	sender := types.Account{Address: trx.From}
	err := db.AddAccountTransaction(&sender, block, trx)
	if err != nil {
		db.log.Error("can not push the transaction to sender account")
		return err
	}

	// do we have a receiving account? may not be present for contract creating transactions
	if trx.To != nil {
		recipient := types.Account{Address: *trx.To}
		err := db.AddAccountTransaction(&recipient, block, trx)
		if err != nil {
			db.log.Error("can not push the transaction to sender account")
			return err
		}
	}

	return nil
}

// isTransactionKnown checks if a transaction document already exists in the database.
func (db *MongoDbBridge) isTransactionKnown(hash *types.Hash) (bool, error) {
	// get the collection for account transactions
	col := db.client.Database(offChainDatabaseName).Collection(coTransactions)

	// try to find the account in the database (it may already exist)
	sr := col.FindOne(context.Background(), bson.D{
		{fiTransactionPk, hash.String()},
	}, options.FindOne().SetProjection(bson.D{{fiTransactionPk, true}}))

	// error on lookup?
	if sr.Err() != nil {
		// may be ErrNoDocuments, which we seek
		if sr.Err() == mongo.ErrNoDocuments {
			return false, nil
		}

		db.log.Error("can not get existing transaction pk")
		return false, sr.Err()
	}

	return true, nil
}

// LastKnownBlock returns number of the last known block stored in the database.
func (db *MongoDbBridge) LastKnownBlock() (uint64, error) {
	// prep search options
	opt := options.FindOne()
	opt.SetSort(bson.D{{fiTransactionBlock, -1}})
	opt.SetProjection(bson.D{{fiTransactionBlock, true}})

	// get the collection for account transactions
	col := db.client.Database(offChainDatabaseName).Collection(coTransactions)
	res := col.FindOne(context.Background(), bson.D{}, opt)
	if res.Err() != nil {
		// may be no block at all
		if res.Err() == mongo.ErrNoDocuments {
			db.log.Info("no blocks found in database")
			return 0, nil
		}

		// log issue
		db.log.Error("can not get the top block")
		return 0, res.Err()
	}

	// get the actual value
	var tx struct {
		Block uint64 `bson:"blk"`
	}

	// get the data
	err := res.Decode(&tx)
	if err != nil {
		db.log.Error("can not decode the top block")
		return 0, res.Err()
	}

	return tx.Block, nil
}

// initTrxList initializes list of transactions based on provided cursor and count.
func (db *MongoDbBridge) initTrxList(col *mongo.Collection, cursor *string, count int32) (*types.TransactionHashList, error) {
	// get the context
	ctx := context.Background()

	// find how many transactions do we have in the database
	total, err := col.CountDocuments(ctx, bson.D{})
	if err != nil {
		db.log.Errorf("can not count transactions")
		return nil, err
	}

	// inform what we are about to do
	db.log.Debugf("found %d transactions in off-chain database", total)

	list := types.TransactionHashList{
		Collection: make([]*types.Hash, 0),
		Total:      uint64(total),
		First:      0,
		Last:       0,
		IsStart:    false,
		IsEnd:      false,
	}

	// db.transaction.createIndex({_id:1,orx:-1},{unique:true, name:"ix-tx-ordinal"})
	// find out the cursor ordinal index
	if cursor == nil && count > 0 {
		// get the highest available ordinal index (top transaction)
		list.First, err = db.findTxOrdinalIndex(col,
			bson.D{},
			options.FindOne().SetSort(bson.D{{fiTransactionOrdinalIndex, -1}}))
		list.IsStart = true

	} else if cursor == nil && count < 0 {
		// get the lowest available ordinal index (top transaction)
		list.First, err = db.findTxOrdinalIndex(col,
			bson.D{},
			options.FindOne().SetSort(bson.D{{fiTransactionOrdinalIndex, 1}}))
		list.IsEnd = true

	} else if cursor != nil {
		// get the highest available ordinal index (top transaction)
		list.First, err = db.findTxOrdinalIndex(col,
			bson.D{{fiTransactionPk, *cursor}},
			options.FindOne())
	}

	// check the error
	if err != nil {
		db.log.Errorf("can not find the initial transactions")
		return nil, err
	}

	// inform what we are about to do
	db.log.Debugf("transaction list initialized with ordinal index %d", list.First)

	return &list, nil
}

// borderTxOrdinalIndex finds the highest, or lowest ordinal index in the transaction database.
// For negative sort it will return highest and for positive sort it will return lowest available value.
func (db *MongoDbBridge) findTxOrdinalIndex(col *mongo.Collection, filter bson.D, opt *options.FindOneOptions) (uint64, error) {
	// prep container
	var row struct {
		Value uint64 `bson:"orx"`
	}

	// make sure we pull only what we need
	opt.SetProjection(bson.D{{fiTransactionOrdinalIndex, true}})
	sr := col.FindOne(context.Background(), filter, opt)

	// try to decode
	err := sr.Decode(&row)
	if err != nil {
		return 0, err
	}

	return row.Value, nil
}

// txListFilter creates a filter for transaction list search.
func (db *MongoDbBridge) txListFilter(cursor *string, count int32, list *types.TransactionHashList) *bson.D {
	// inform what we are about to do
	db.log.Debugf("transaction filter starts from index %d", list.First)

	// build the filter query
	var filter bson.D
	if cursor == nil {
		if count > 0 {
			filter = bson.D{{fiTransactionOrdinalIndex, bson.D{{"$lte", list.First}}}}
		} else {
			filter = bson.D{{fiTransactionOrdinalIndex, bson.D{{"$gte", list.First}}}}
		}
	} else {
		if count > 0 {
			filter = bson.D{{fiTransactionOrdinalIndex, bson.D{{"$lt", list.First}}}}
		} else {
			filter = bson.D{{fiTransactionOrdinalIndex, bson.D{{"$gt", list.First}}}}
		}
	}

	return &filter
}

// txListOptions creates a filter options set for transactions list search.
func (db *MongoDbBridge) txListOptions(count int32) *options.FindOptions {
	// prep options
	opt := options.Find()
	opt.SetProjection(bson.D{{fiTransactionPk, true}, {fiTransactionOrdinalIndex, true}})

	// how to sort results in the collection
	if count > 0 {
		// from high (new) to low (old)
		opt.SetSort(bson.D{{fiTransactionOrdinalIndex, -1}})
	} else {
		// from low (old) to high (new)
		opt.SetSort(bson.D{{fiTransactionOrdinalIndex, 1}})
	}

	// prep the loading limit
	var limit = int64(count)
	if limit < 0 {
		limit = -limit
	}

	// try to get one more
	limit++

	// apply the limit
	opt.SetLimit(limit)

	return opt
}

// txListLoad load the initialized list from database
func (db *MongoDbBridge) txListLoad(col *mongo.Collection, cursor *string, count int32, list *types.TransactionHashList) error {
	// get the context for loader
	ctx := context.Background()

	// load the data
	ld, err := col.Find(ctx, db.txListFilter(cursor, count, list), db.txListOptions(count))
	if err != nil {
		db.log.Errorf("error loading transactions list; %s", err.Error())
		return err
	}

	// close the cursor as we leave
	defer func() {
		err := ld.Close(ctx)
		if err != nil {
			db.log.Errorf("error closing transactions list cursor; %s", err.Error())
		}
	}()

	// loop and load
	var hash *types.Hash
	for ld.Next(ctx) {
		// process the last found hash
		if hash != nil {
			list.Collection = append(list.Collection, hash)
		}

		// get the next hash
		var row struct {
			Id  string `bson:"_id"`
			Orx uint64 `bson:"orx"`
		}

		// try to decode the next row
		if err := ld.Decode(&row); err != nil {
			db.log.Errorf("can not decode the list row; %s", err.Error())
			return err
		}

		// decode the value
		h := types.HexToHash(row.Id)
		hash = &h
	}

	// we should have all the items already; we may just need to check if a boundary was reached
	if cursor != nil {
		list.IsEnd = count > 0 && int32(len(list.Collection)) < count
		list.IsStart = count < 0 && int32(len(list.Collection)) < -count

		// add the last item as well
		if (list.IsStart || list.IsEnd) && hash != nil {
			list.Collection = append(list.Collection, hash)
		}
	}

	return nil
}

// Transactions pulls list of transaction hashes starting on the specified cursor.
func (db *MongoDbBridge) Transactions(cursor *string, count int32) (*types.TransactionHashList, error) {
	// nothing to load?
	if count == 0 {
		return nil, fmt.Errorf("nothing to do, zero transactions requested")
	}

	// get the collection and context
	col := db.client.Database(offChainDatabaseName).Collection(coTransactions)

	// init the list
	list, err := db.initTrxList(col, cursor, count)
	if err != nil {
		db.log.Errorf("can not build transactions list; %s", err.Error())
		return nil, err
	}

	// load data
	err = db.txListLoad(col, cursor, count, list)
	if err != nil {
		db.log.Errorf("can not load transactions list from database; %s", err.Error())
		return nil, err
	}

	// reverse on negative so new-er transaction will be on top
	if count < 0 {
		list.Reverse()
	}

	return list, nil
}
