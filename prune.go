// Copyright 2015 Canonical Ltd.
// Licensed under the LGPLv3, see LICENCE file for details.

package txn

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/juju/errors"
	"github.com/juju/mgo/v3"
	"github.com/juju/mgo/v3/bson"
)

const (
	// Transaction states copied from mgo/txn.
	taborted = 5 // Pre-conditions failed, nothing done
	tapplied = 6 // All changes applied

	// maxBatchDocs defines the maximum MongoDB batch size (in number of documents).
	maxBatchDocs = 1616

	// defaultPruneFactor will be used if users don't request a pruneFactor
	defaultPruneFactor = 2.0

	// defaultMinNewTransactions will avoid pruning if there are only a
	// small number of documents to prune. This is set because if a
	// database can get to 0 txns, then any pruneFactor will always say
	// that we should prune.
	defaultMinNewTransactions = 100

	// defaultMaxNewTransactions will trigger a prune if we see more than
	// this many new transactions, even if pruneFactor hasn't been satisfied
	defaultMaxNewTransactions = 100000

	// defaultSmallBatchTransaction count represents a tradeoff of how many
	// transactions we will read, and then lookup all the docs that they
	// reference as a group. Increasing this should improve the efficiency
	// of document reading, at a cost of larger batches. Empirically, 1000
	// seems to be a good balance. A large batch also represents a larger
	// 'Remove' query against the txns collection.
	defaultSmallBatchTransactionCount = 1000

	// defaultBatchTransactionSleepTime represents approximately a 10% cycle time.
	// generally a batch of 1000 txns takes around 100ms to process. By sleeping
	// for 10ms, that slows us down by ~10%, but means that other queries have
	// opportunities to get queries in.
	defaultBatchTransactionSleepTime = 10 * time.Millisecond

	// maxBulkOps defines the maximum number of operations in a bulk
	// operation.
	maxBulkOps = 1000

	// logInterval defines often to report progress during long
	// operations.
	logInterval = 15 * time.Second

	// maxIterCount is the number of times we will pass over the data to
	// make sure all documents are cleaned up. (removing from a
	// collection you are iterating can cause you to miss entries).
	// The loop should exit early if it finds nothing to do anyway, so
	// this only affects the number of times we will evaluate documents
	// we aren't removing.
	maxIterCount = 5

	// maxMemoryTokens caps our in-memory cache. When it is full, we will
	// apply our current list of items to process, and then flag the loop
	// to run again. At 100k the maximum memory was around 200MB.
	maxMemoryTokens = 50000

	// queueBatchSize is the number of documents we will load before
	// evaluating their transaction queues. This was found to be
	// reasonably optimal when querying mongo.
	queueBatchSize = 200
)

type pruneStats struct {
	Id              bson.ObjectId `bson:"_id"`
	Started         time.Time     `bson:"started"`
	Completed       time.Time     `bson:"completed"`
	TxnsBefore      int           `bson:"txns-before"`
	TxnsAfter       int           `bson:"txns-after"`
	StashDocsBefore int           `bson:"stash-docs-before"`
	StashDocsAfter  int           `bson:"stash-docs-after"`
}

func validatePruneOptions(pruneOptions *PruneOptions) {
	if pruneOptions.PruneFactor == 0 {
		pruneOptions.PruneFactor = defaultPruneFactor
	}
	if pruneOptions.MinNewTransactions == 0 {
		pruneOptions.MinNewTransactions = defaultMinNewTransactions
	}
	if pruneOptions.MaxNewTransactions == 0 {
		pruneOptions.MaxNewTransactions = defaultMaxNewTransactions
	}
	if pruneOptions.MaxBatches <= 0 {
		pruneOptions.MaxBatches = 1
	}
	if pruneOptions.BatchTransactionSleepTime < 0 {
		pruneOptions.BatchTransactionSleepTime = defaultBatchTransactionSleepTime
	}
	if pruneOptions.SmallBatchTransactionCount < pruneMinTxnBatchSize {
		pruneOptions.SmallBatchTransactionCount = defaultSmallBatchTransactionCount
	}
}

func shouldPrune(oldCount, newCount int, pruneOptions PruneOptions) (bool, string) {
	if oldCount < 0 {
		return true, "no pruning run found"
	}
	difference := newCount - oldCount
	if difference < pruneOptions.MinNewTransactions {
		return false, "not enough new transactions"
	}
	if difference > pruneOptions.MaxNewTransactions {
		return true, "too many new transactions"
	}
	factored := float32(oldCount) * pruneOptions.PruneFactor
	if float32(newCount) >= factored {
		return true, "transactions have grown significantly"
	}
	return false, "transactions have not grown significantly"
}

func maybePrune(db *mgo.Database, txnsName string, pruneOpts PruneOptions) error {
	validatePruneOptions(&pruneOpts)
	logger.Debugf("validated pruneOpts: %#v", pruneOpts)
	txnsPrune := db.C(txnsPruneC(txnsName))
	txns := db.C(txnsName)
	txnsStashName := txnsName + ".stash"
	txnsStash := db.C(txnsStashName)

	txnsCount, err := txns.Count()
	if err != nil {
		return fmt.Errorf("failed to retrieve starting txns count: %v", err)
	}
	lastTxnsCount, err := getPruneLastTxnsCount(txnsPrune)
	if err != nil {
		return fmt.Errorf("failed to retrieve pruning stats: %v", err)
	}

	required, rationale := shouldPrune(lastTxnsCount, txnsCount, pruneOpts)

	if !required {
		logger.Infof("txns after last prune: %d, txns now: %d, not pruning: %s",
			lastTxnsCount, txnsCount, rationale)
		return nil
	}
	logger.Infof("txns after last prune: %d, txns now: %d, pruning: %s",
		lastTxnsCount, txnsCount, rationale)
	started := time.Now()

	stashDocsBefore, err := txnsStash.Count()
	if err != nil {
		return fmt.Errorf("failed to retrieve starting %q count: %v", txnsStashName, err)
	}

	txnsCountBefore := txnsCount
	session := txns.Database.Session.Copy()
	defer session.Close()
	localTxns := txns.With(session)
	stats, err := CleanAndPrune(CleanAndPruneArgs{
		Txns:                     localTxns,
		TxnsCount:                txnsCount,
		MaxTime:                  pruneOpts.MaxTime,
		MaxTransactionsToProcess: pruneOpts.MaxBatchTransactions,
		TxnBatchSize:             pruneOpts.SmallBatchTransactionCount,
		TxnBatchSleepTime:        pruneOpts.BatchTransactionSleepTime,
	})
	if err != nil {
		return errors.Trace(err)
	}
	txnsCountAfter, err := txns.Count()
	if err != nil {
		return fmt.Errorf("failed to retrieve final txns count: %v", err)
	}
	stashDocsAfter, err := txnsStash.Count()
	if err != nil {
		return fmt.Errorf("failed to retrieve final %q count: %v", txnsStashName, err)
	}
	elapsed := time.Since(started)
	logger.Infof("txn pruning complete after %v. txns now: %d, inspected %d collections, %d docs (%d cleaned)\n   removed %d stash docs and %d txn docs",
		elapsed, txnsCountAfter, stats.CollectionsInspected, stats.DocsInspected, stats.DocsCleaned, stats.StashDocumentsRemoved, stats.TransactionsRemoved)
	completed := time.Now()
	return writePruneTxnsCount(txnsPrune, started, completed, txnsCountBefore, txnsCountAfter,
		stashDocsBefore, stashDocsAfter)
}

// CleanAndPruneArgs specifies the parameters required by CleanAndPrune.
type CleanAndPruneArgs struct {

	// Txns is the collection that holds all of the transactions that we
	// might want to prune. We will also make use of Txns.Database to find
	// all of the collections that might make use of transactions from that
	// collection.
	Txns *mgo.Collection

	// TxnsCount is a hint from Txns.Count() to avoid having to call it again
	// to determine whether it is ok to hold the set of transactions in memory.
	// It is optional, as we will call Txns.Count() if it is not supplied.
	TxnsCount int

	// MaxTime is a timestamp that provides a threshold of transactions
	// that we will actually prune. Only transactions that were created
	// before this threshold will be pruned.
	MaxTime time.Time

	// MaxTransactionsToProcess defines how many completed transactions that we will evaluate in this batch.
	// A value of 0 indicates we should evaluate all completed transactions.
	MaxTransactionsToProcess int

	// Multithreaded will start multiple pruning passes concurrently
	Multithreaded bool

	// TxnBatchSize is how many transaction to process at once.
	TxnBatchSize int

	// TxnBatchSleepTime is how long we should sleep between processing transaction
	// batches, to allow other parts of the system to operate (avoid consuming
	// all resources)
	// The default is to not sleep at all, but this can be configured to reduce
	// load while pruning.
	TxnBatchSleepTime time.Duration
}

func (args *CleanAndPruneArgs) validate() error {
	if args.Txns == nil {
		return errors.New("nil Txns not valid")
	}
	if args.TxnBatchSleepTime < 0 || args.TxnBatchSleepTime > maxBatchSleepTime {
		return errors.Errorf("TxnBatchSleepTime (%s) must be between 0s and %s",
			args.TxnBatchSleepTime, maxBatchSleepTime)
	}
	// A value of 0 indicates that we should use the default as it hasn't been set
	if args.TxnBatchSize == 0 {
		args.TxnBatchSize = pruneTxnBatchSize
	}
	if args.TxnBatchSize < pruneMinTxnBatchSize {
		return errors.Errorf("TxnBatchSize %d too small, must be between %d and %d",
			args.TxnBatchSize, pruneMinTxnBatchSize, pruneMaxTxnBatchSize)
	}
	if args.TxnBatchSize > pruneMaxTxnBatchSize {
		return errors.Errorf("TxnBatchSize %d too big, must be between %d and %d",
			args.TxnBatchSize, pruneMinTxnBatchSize, pruneMaxTxnBatchSize)
	}
	return nil
}

// CleanupStats gives some numbers as to what work was done as part of
// CleanupAndPrune.
type CleanupStats struct {

	// CollectionsInspected is the total number of collections we looked at for documents
	CollectionsInspected int

	// DocsInspected is how many documents we loaded to evaluate their txn queues
	DocsInspected int

	// DocsCleaned is how many documents we Updated to remove entries from their txn queue.
	DocsCleaned int

	// StashDocumentsRemoved is how many total documents we remove from txns.stash
	StashDocumentsRemoved int

	// StashDocumentsRemoved is how many documents we remove from txns
	TransactionsRemoved int

	// ShouldRetry indicates that we think this cleanup was not complete due to too many txns to process. We recommend running it again.
	ShouldRetry bool
}

func startReportingThread(stop <-chan struct{}, progressCh chan ProgressMessage) {
	tStart := time.Now()
	next := time.After(15 * time.Second)
	go func() {
		txnsRemoved := 0
		docsCleaned := 0
		for {
			select {
			case <-stop:
				return
			case msg := <-progressCh:
				txnsRemoved += msg.TxnsRemoved
				docsCleaned += msg.DocsCleaned
			case <-next:
				txnRate := 0.0
				since := time.Since(tStart).Seconds()
				if since > 0 {
					txnRate = float64(txnsRemoved) / since
				}
				logger.Debugf("pruning has removed %d txns (%.0ftxn/s) cleaning %d docs ",
					txnsRemoved, txnRate, docsCleaned)
				next = time.After(15 * time.Second)
			}
		}
	}()
}

// CleanAndPrune runs the cleanup steps, and then follows up with pruning all
// of the transactions that are no longer referenced.
func CleanAndPrune(args CleanAndPruneArgs) (CleanupStats, error) {
	tStart := time.Now()
	var stats CleanupStats

	if err := args.validate(); err != nil {
		return stats, err
	}
	stop := make(chan struct{})
	progressCh := make(chan ProgressMessage)
	startReportingThread(stop, progressCh)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var pstats PrunerStats
	var anyErr error
	prune := func(reversed bool) {
		pruner := NewIncrementalPruner(IncrementalPruneArgs{
			MaxTime:           args.MaxTime,
			ProgressChannel:   progressCh,
			ReverseOrder:      reversed,
			TxnBatchSize:      args.TxnBatchSize,
			TxnBatchSleepTime: args.TxnBatchSleepTime,
		})
		thisPstats, err := pruner.Prune(args.Txns)
		mu.Lock()
		pstats = CombineStats(pstats, thisPstats)
		if anyErr == nil {
			anyErr = errors.Trace(err)
		} else if err != nil {
			logger.Warningf("second error while handling initial error: %v", err)
		}
		mu.Unlock()
		wg.Done()
	}
	if args.Multithreaded {
		wg.Add(1)
		go prune(true)
	}
	wg.Add(1)
	prune(false)
	wg.Wait()
	close(stop)
	if anyErr != nil {
		return stats, errors.Trace(anyErr)
	}
	logger.Infof("pruning removed %d txns and cleaned %d docs in %s.",
		pstats.TxnsRemoved,
		pstats.DocQueuesCleaned,
		time.Since(tStart).Round(time.Millisecond))
	logger.Debugf("%s", pstats)
	stats.TransactionsRemoved = int(pstats.TxnsRemoved)
	stats.DocsCleaned = int(pstats.DocQueuesCleaned)
	stats.StashDocumentsRemoved = int(pstats.StashDocsRemoved)
	stats.DocsInspected = int(pstats.DocCacheMisses + pstats.DocCacheHits)
	stats.CollectionsInspected = int(pstats.CollectionQueries)
	return stats, nil
}

// getPruneLastTxnsCount will return how many documents were in 'txns' the
// last time we pruned. It will return -1 if it cannot find a reliable value
// (no value available, or corrupted document.)
func getPruneLastTxnsCount(txnsPrune *mgo.Collection) (int, error) {
	// Retrieve the doc which points to the latest stats entry.
	var ptrDoc bson.M
	err := txnsPrune.FindId("last").One(&ptrDoc)
	if err == mgo.ErrNotFound {
		return -1, nil
	} else if err != nil {
		return -1, fmt.Errorf("failed to load pruning stats pointer: %v", err)
	}

	// Get the stats.
	var doc pruneStats
	err = txnsPrune.FindId(ptrDoc["id"]).One(&doc)
	if err == mgo.ErrNotFound {
		// Pointer was broken. Recover by returning 0 which will force
		// pruning.
		logger.Warningf("pruning stats pointer was broken - will recover")
		return -1, nil
	} else if err != nil {
		return -1, fmt.Errorf("failed to load pruning stats: %v", err)
	}
	return doc.TxnsAfter, nil
}

func writePruneTxnsCount(
	txnsPrune *mgo.Collection,
	started, completed time.Time,
	txnsBefore, txnsAfter,
	stashBefore, stashAfter int,
) error {
	id := bson.NewObjectId()
	err := txnsPrune.Insert(pruneStats{
		Id:              id,
		Started:         started,
		Completed:       completed,
		TxnsBefore:      txnsBefore,
		TxnsAfter:       txnsAfter,
		StashDocsBefore: stashBefore,
		StashDocsAfter:  stashAfter,
	})
	if err != nil {
		return fmt.Errorf("failed to write prune stats: %v", err)
	}

	// Set pointer to latest stats document.
	_, err = txnsPrune.UpsertId("last", bson.M{"$set": bson.M{"id": id}})
	if err != nil {
		return fmt.Errorf("failed to write prune stats pointer: %v", err)
	}
	return nil
}

func txnsPruneC(txnsName string) string {
	return txnsName + ".prune"
}

// txnCollections takes the list of all collections in a database and
// filters them to just the ones that may have txn references.
func txnCollections(inNames []string, txnsName string) []string {
	// hasTxnReferences returns true if a collection may have
	// references to txns.
	hasTxnReferences := func(name string) bool {
		switch {
		case name == txnsName+".stash":
			return true // Need to look in the stash.
		case name == txnsName, strings.HasPrefix(name, txnsName+"."):
			// The txns collection and its children shouldn't be considered.
			return false
		case name == "statuseshistory":
			// statuseshistory is a special case that doesn't use txn and does get fairly big, so skip it
			return false
		case strings.HasPrefix(name, "system."):
			// Don't look in system collections.
			return false
		default:
			// Everything else needs to be considered.
			return true
		}
	}

	outNames := make([]string, 0, len(inNames))
	for _, name := range inNames {
		if hasTxnReferences(name) {
			outNames = append(outNames, name)
		}
	}
	return outNames
}

func txnTokenToId(token string) bson.ObjectId {
	// mgo/txn transaction tokens are the 24 character txn id
	// followed by "_<nonce>"
	return bson.ObjectIdHex(token[:24])
}

func newBatchRemover(coll *mgo.Collection) *batchRemover {
	return &batchRemover{
		coll: coll,
	}
}

type Remover interface {
	Remove(id interface{}) error
	Flush() error
	Removed() int
}

type batchRemover struct {
	coll    *mgo.Collection
	queue   []interface{}
	removed int
}

var _ Remover = (*batchRemover)(nil)

func (r *batchRemover) Remove(id interface{}) error {
	r.queue = append(r.queue, id)
	if len(r.queue) >= maxBulkOps {
		return r.Flush()
	}
	return nil
}

func (r *batchRemover) Flush() error {
	if len(r.queue) < 1 {
		return nil // Nothing to do
	}
	filter := bson.M{"_id": bson.M{"$in": r.queue}}
	switch result, err := r.coll.RemoveAll(filter); err {
	case nil, mgo.ErrNotFound:
		// It's OK for txns to no longer exist. Another process
		// may have concurrently pruned them.
		r.removed += result.Removed
		r.queue = r.queue[:0]
		return nil
	default:
		return err
	}
}

func (r *batchRemover) Removed() int {
	return r.removed
}

func newBulkRemover(coll *mgo.Collection) *bulkRemover {
	r := &bulkRemover{coll: coll}
	r.newChunk()
	return r
}

type bulkRemover struct {
	coll      *mgo.Collection
	chunk     *mgo.Bulk
	chunkSize int
	removed   int
}

var _ Remover = (*bulkRemover)(nil)

func (r *bulkRemover) newChunk() {
	r.chunk = r.coll.Bulk()
	r.chunk.Unordered()
	r.chunkSize = 0
}

func (r *bulkRemover) Remove(id interface{}) error {
	r.chunk.Remove(bson.D{{"_id", id}})
	r.chunkSize++
	if r.chunkSize >= maxBulkOps {
		return r.Flush()
	}
	return nil
}

func (r *bulkRemover) Flush() error {
	if r.chunkSize < 1 {
		return nil // Nothing to do
	}
	switch result, err := r.chunk.Run(); err {
	case nil, mgo.ErrNotFound:
		// It's OK for txns to no longer exist. Another process
		// may have concurrently pruned them.
		if result != nil {
			r.removed += result.Matched
		}
		r.newChunk()
		return nil
	default:
		return err
	}
}

func (r *bulkRemover) Removed() int {
	return r.removed
}

func newSimpleTimer(interval time.Duration) *simpleTimer {
	return &simpleTimer{
		interval: interval,
		next:     time.Now().Add(interval),
	}
}

type simpleTimer struct {
	interval time.Duration
	next     time.Time
}

func (t *simpleTimer) isAfter() bool {
	now := time.Now()
	if now.After(t.next) {
		t.next = now.Add(t.interval)
		return true
	}
	return false
}
