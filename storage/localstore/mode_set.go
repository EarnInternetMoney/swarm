// Copyright 2018 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package localstore

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethersphere/swarm/chunk"
	"github.com/syndtr/goleveldb/leveldb"
)

// Set updates database indexes for a specific
// chunk represented by the address.
// Set is required to implement chunk.Store
// interface.
func (db *DB) Set(ctx context.Context, mode chunk.ModeSet, addr chunk.Address) (err error) {
	metricName := fmt.Sprintf("localstore.Set.%s", mode)

	metrics.GetOrRegisterCounter(metricName, nil).Inc(1)
	defer totalTimeMetric(metricName, time.Now())

	err = db.set(mode, addr)
	if err != nil {
		metrics.GetOrRegisterCounter(metricName+".error", nil).Inc(1)
	}
	return err
}

// set updates database indexes for a specific
// chunk represented by the address.
// It acquires lockAddr to protect two calls
// of this function for the same address in parallel.
func (db *DB) set(mode chunk.ModeSet, addr chunk.Address) (err error) {
	// protect parallel updates
	db.batchMu.Lock()
	defer db.batchMu.Unlock()

	batch := new(leveldb.Batch)

	// variables that provide information for operations
	// to be done after write batch function successfully executes
	var gcSizeChange int64   // number to add or subtract from gcSize
	var triggerPullFeed bool // signal pull feed subscriptions to iterate

	item := addressToItem(addr)

	switch mode {
	case chunk.ModeSetAccess:
		// add to pull, insert to gc

		// need to get access timestamp here as it is not
		// provided by the access function, and it is not
		// a property of a chunk provided to Accessor.Put.

		i, err := db.retrievalDataIndex.Get(item)
		switch err {
		case nil:
			item.StoreTimestamp = i.StoreTimestamp
			item.BinID = i.BinID
		case leveldb.ErrNotFound:
			db.pushIndex.DeleteInBatch(batch, item)
			item.StoreTimestamp = now()
			item.BinID, err = db.binIDs.Inc(uint64(db.po(item.Address)))
			if err != nil {
				return err
			}
		default:
			return err
		}

		i, err = db.retrievalAccessIndex.Get(item)
		switch err {
		case nil:
			item.AccessTimestamp = i.AccessTimestamp
			db.gcIndex.DeleteInBatch(batch, item)
			gcSizeChange--
		case leveldb.ErrNotFound:
			// the chunk is not accessed before
		default:
			return err
		}
		item.AccessTimestamp = now()
		db.retrievalAccessIndex.PutInBatch(batch, item)
		db.pullIndex.PutInBatch(batch, item)
		triggerPullFeed = true
		db.gcIndex.PutInBatch(batch, item)
		gcSizeChange++

	case chunk.ModeSetSync:
		// delete from push, insert to gc

		// need to get access timestamp here as it is not
		// provided by the access function, and it is not
		// a property of a chunk provided to Accessor.Put.
		i, err := db.retrievalDataIndex.Get(item)
		if err != nil {
			if err == leveldb.ErrNotFound {
				// chunk is not found,
				// no need to update gc index
				// just delete from the push index
				// if it is there
				db.pushIndex.DeleteInBatch(batch, item)
				return nil
			}
			return err
		}
		item.StoreTimestamp = i.StoreTimestamp
		item.BinID = i.BinID

		i, err = db.retrievalAccessIndex.Get(item)
		switch err {
		case nil:
			item.AccessTimestamp = i.AccessTimestamp
			db.gcIndex.DeleteInBatch(batch, item)
			gcSizeChange--
		case leveldb.ErrNotFound:
			// the chunk is not accessed before
		default:
			return err
		}
		item.AccessTimestamp = now()
		db.retrievalAccessIndex.PutInBatch(batch, item)
		db.pushIndex.DeleteInBatch(batch, item)

		// Add in gcIndex only if this chunk is not pinned
		ok, err := db.pinIndex.Has(item)
		if err != nil {
			return err
		}
		if !ok {
			db.gcIndex.PutInBatch(batch, item)
			gcSizeChange++
		}
	case chunk.ModeSetRemove:
		// delete from retrieve, pull, gc

		// need to get access timestamp here as it is not
		// provided by the access function, and it is not
		// a property of a chunk provided to Accessor.Put.

		i, err := db.retrievalAccessIndex.Get(item)
		switch err {
		case nil:
			item.AccessTimestamp = i.AccessTimestamp
		case leveldb.ErrNotFound:
		default:
			return err
		}
		i, err = db.retrievalDataIndex.Get(item)
		if err != nil {
			return err
		}
		item.StoreTimestamp = i.StoreTimestamp
		item.BinID = i.BinID

		db.retrievalDataIndex.DeleteInBatch(batch, item)
		db.retrievalAccessIndex.DeleteInBatch(batch, item)
		db.pullIndex.DeleteInBatch(batch, item)
		db.gcIndex.DeleteInBatch(batch, item)
		// a check is needed for decrementing gcSize
		// as delete is not reporting if the key/value pair
		// is deleted or not
		if _, err := db.gcIndex.Get(item); err == nil {
			gcSizeChange = -1
		}

	case chunk.ModeSetPin:
		// Get the existing pin counter of the chunk
		existingPinCounter := uint64(0)
		pinnedChunk, err := db.pinIndex.Get(item)
		if err != nil {
			if err == leveldb.ErrNotFound {
				// If this Address is not present in DB, then its a new entry
				existingPinCounter = 0

				// Add in gcExcludeIndex of the chunk is not pinned already
				db.gcExcludeIndex.PutInBatch(batch, item)
			} else {
				return err
			}
		} else {
			existingPinCounter = pinnedChunk.PinCounter
		}

		// Otherwise increase the existing counter by 1
		item.PinCounter = existingPinCounter + 1
		db.pinIndex.PutInBatch(batch, item)
	case chunk.ModeSetUnpin:
		// Get the existing pin counter of the chunk
		pinnedChunk, err := db.pinIndex.Get(item)
		if err != nil {
			return err
		}

		// Decrement the pin counter or
		// delete it from pin index if the pin counter has reached 0
		if pinnedChunk.PinCounter > 1 {
			item.PinCounter = pinnedChunk.PinCounter - 1
			db.pinIndex.PutInBatch(batch, item)
		} else {
			db.pinIndex.DeleteInBatch(batch, item)
		}
	default:
		return ErrInvalidMode
	}

	err = db.incGCSizeInBatch(batch, gcSizeChange)
	if err != nil {
		return err
	}

	err = db.shed.WriteBatch(batch)
	if err != nil {
		return err
	}
	if triggerPullFeed {
		db.triggerPullSubscriptions(db.po(item.Address))
	}
	return nil
}