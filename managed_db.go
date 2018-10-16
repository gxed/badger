/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/y"
	"github.com/pkg/errors"
)

// OpenManaged returns a new DB, which allows more control over setting
// transaction timestamps, by setting managedDB=true.
//
// This is only useful for databases built on top of Badger (like Dgraph), and
// can be ignored by most users.
func OpenManaged(opts Options) (*DB, error) {
	opts.managedTxns = true
	return Open(opts)
}

// NewTransactionAt follows the same logic as DB.NewTransaction(), but uses the
// provided read timestamp.
//
// This is only useful for databases built on top of Badger (like Dgraph), and
// can be ignored by most users.
func (db *DB) NewTransactionAt(readTs uint64, update bool) *Txn {
	if !db.opt.managedTxns {
		panic("Cannot use NewTransactionAt with managedDB=false. Use NewTransaction instead.")
	}
	txn := db.newTransaction(update, true)
	txn.readTs = readTs
	return txn
}

// CommitAt commits the transaction, following the same logic as Commit(), but
// at the given commit timestamp. This will panic if not used with managed transactions.
//
// This is only useful for databases built on top of Badger (like Dgraph), and
// can be ignored by most users.
func (txn *Txn) CommitAt(commitTs uint64, callback func(error)) error {
	if !txn.db.opt.managedTxns {
		panic("Cannot use CommitAt with managedDB=false. Use Commit instead.")
	}
	txn.commitTs = commitTs
	if callback == nil {
		return txn.Commit()
	}
	txn.CommitWith(callback)
	return nil
}

// SetDiscardTs sets a timestamp at or below which, any invalid or deleted
// versions can be discarded from the LSM tree, and thence from the value log to
// reclaim disk space. Can only be used with managed transactions.
func (db *DB) SetDiscardTs(ts uint64) {
	if !db.opt.managedTxns {
		panic("Cannot use SetDiscardTs with managedDB=false.")
	}
	db.orc.setDiscardTs(ts)
}

var errDone = errors.New("Done deleting keys")

// DropAll would drop all the data stored in Badger. It does this in the following way.
// - Stop accepting new writes.
// - Pause the compactions.
// - Pick all tables from all levels, create a changeset to delete all these
// tables and apply it to manifest. DO not pick up the latest table from level
// 0, to preserve the (persistent) badgerHead key.
// - Iterate over the KVs in Level 0, and run deletes on them via transactions.
// - The deletions are done at the same timestamp as the latest version of the
// key. Thus, we could write the keys back at the same timestamp as before.
//
// DropAll is only available with managed transactions.
func (db *DB) DropAll() error {
	if !db.opt.managedTxns {
		panic("DropAll is only available with managedDB=true.")
	}
	// Stop accepting new writes.
	atomic.StoreInt32(&db.blockWrites, 1)

	// Wait for writeCh to reach size of zero. This is not ideal, but a very
	// simple way to allow writeCh to flush out, before we proceed.
	tick := time.NewTicker(100 * time.Millisecond)
	for range tick.C {
		if len(db.writeCh) == 0 {
			break
		}
	}
	tick.Stop()

	// Stop the compactions.
	if db.closers.compactors != nil {
		db.closers.compactors.SignalAndWait()
	}

	_, err := db.lc.deleteLSMTree()
	// Allow writes so that we can run transactions. Ideally, the user must ensure that they're not
	// doing more writes concurrently while this operation is happening.
	atomic.StoreInt32(&db.blockWrites, 0)
	// Need compactions to happen so deletes below can be flushed out.
	if db.closers.compactors != nil {
		db.closers.compactors = y.NewCloser(1)
		db.lc.startCompact(db.closers.compactors)
	}
	if err != nil {
		return err
	}

	type KV struct {
		key     []byte
		version uint64
	}

	var kvs []KV
	getKeys := func() error {
		txn := db.NewTransactionAt(math.MaxUint64, false)
		defer txn.Discard()

		opts := DefaultIteratorOptions
		opts.PrefetchValues = false
		itr := txn.NewIterator(opts)
		defer itr.Close()

		for itr.Rewind(); itr.Valid(); itr.Next() {
			item := itr.Item()
			kvs = append(kvs, KV{item.KeyCopy(nil), item.Version()})
		}
		return nil
	}
	if err := getKeys(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for _, kv := range kvs {
		wg.Add(1)
		txn := db.NewTransactionAt(math.MaxUint64, true)
		if err := txn.Delete(kv.key); err != nil {
			return err
		}
		if err := txn.CommitAt(kv.version, func(rerr error) {
			if rerr != nil {
				select {
				case errCh <- rerr:
				default:
				}
			}
			wg.Done()
		}); err != nil {
			return err
		}
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}
