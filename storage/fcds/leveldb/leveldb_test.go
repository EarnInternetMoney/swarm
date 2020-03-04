// Copyright 2019 The Swarm Authors
// This file is part of the Swarm library.
//
// The Swarm library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Swarm library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Swarm library. If not, see <http://www.gnu.org/licenses/>.

package leveldb_test

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethersphere/swarm/chunk"
	chunktesting "github.com/ethersphere/swarm/chunk/testing"
	"github.com/ethersphere/swarm/storage/fcds"
	"github.com/ethersphere/swarm/storage/fcds/leveldb"
	"github.com/ethersphere/swarm/storage/fcds/test"
)

// TestFCDS runs a standard series of tests on main Store implementation
// with LevelDB meta store.
func TestFCDS(t *testing.T) {
	test.RunAll(t, func(t *testing.T) (fcds.Storer, func()) {
		path, err := ioutil.TempDir("", "swarm-fcds-")
		if err != nil {
			t.Fatal(err)
		}

		metaStore, err := leveldb.NewMetaStore(filepath.Join(path, "meta"))
		if err != nil {
			t.Fatal(err)
		}

		return test.NewFCDSStore(t, path, metaStore)
	})
}

// TestFreeSlotCounter tests that the free slot counter gets persisted
// and properly loaded on existing store restart
func TestFreeSlotCounter(t *testing.T) {
	path, err := ioutil.TempDir("", "swarm-fcds-")
	if err != nil {
		t.Fatal(err)
	}

	metaPath := filepath.Join(path, "meta")

	metaStore, err := leveldb.NewMetaStore(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	store, err := fcds.New(path, chunk.DefaultSize, metaStore, fcds.WithCache(false))
	if err != nil {
		os.RemoveAll(path)
		t.Fatal(err)
	}

	defer func() {
		store.Close()
		os.RemoveAll(path)
	}()

	// put some chunks, delete some chunks, find the free slots
	// then close the store, init a new one on the same dir
	// then check free slots again and compare
	numChunks := 100
	deleteChunks := 10
	chunks := make([]chunk.Chunk, numChunks)

	for i := 0; i < numChunks; i++ {
		chunks[i] = chunktesting.GenerateTestRandomChunk()
		_, err := store.Put(chunks[i])
		if err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < deleteChunks; i++ {
		err := store.Delete(chunks[i].Address())
		if err != nil {
			t.Fatal(err)
		}
	}

	freeSlots := metaStore.ShardSlots()

	store.Close()
	metaStore.Close()

	metaStore2, err := leveldb.NewMetaStore(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		metaStore2.Close()
		os.RemoveAll(metaPath)
	}()

	freeSlots2 := metaStore.ShardSlots()
	count := 0
	for i, v := range freeSlots {
		count++
		if freeSlots2[i].Shard != v.Shard {
			t.Fatalf("expected shard %d to be %d but got %d", i, v.Shard, freeSlots[2].Shard)
		}
		if freeSlots2[i].Slots != v.Slots {
			t.Fatalf("expected shard %d to have %d free slots but got %d", i, v.Slots, freeSlots[2].Slots)
		}
	}

	if uint8(count) != fcds.ShardCount {
		t.Fatalf("did not process enough shards: got %d but expected %d", count, fcds.ShardCount)
	}
}

func TestIssue1(t *testing.T) {
	path, err := ioutil.TempDir("", "swarm-fcds-")
	if err != nil {
		t.Fatal(err)
	}

	fmt.Println(path)

	metaStore, err := leveldb.NewMetaStore(filepath.Join(path, "meta"))
	if err != nil {
		t.Fatal(err)
	}

	s, cleanup := test.NewFCDSStore(t, path, metaStore)
	defer cleanup()

	var wg sync.WaitGroup

	var mu sync.Mutex
	addrs := make(map[string]struct{})
	trigger := make(chan struct{}, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()

		sem := make(chan struct{}, 100)

		for i := 0; i < 100000; i++ {
			i := i
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				ch := chunktesting.GenerateTestRandomChunk()
				_, err := s.Put(ch)
				if err != nil {
					panic(err)
				}
				if i%10 == 0 {
					// THIS IS CAUSING THE ISSUE
					// every tenth chunk write again after some time
					go func() {
						time.Sleep(10 * time.Second)
						_, err := s.Put(ch)
						if err != nil {
							panic(err)
						}
						mu.Lock()
						addrs[ch.Address().String()] = struct{}{}
						mu.Unlock()
					}()
				}
				mu.Lock()
				addrs[ch.Address().String()] = struct{}{}
				if len(addrs) >= 1000 {
					select {
					case trigger <- struct{}{}:
					default:
					}
				}
				if i%100 == 0 {
					size, err := dirSize(path)
					if err != nil {
						panic(err)
					}
					fmt.Println("r", i, size, len(addrs))
				}
				mu.Unlock()
				time.Sleep(time.Duration(rand.Intn(300)) * time.Millisecond)
			}()
		}
	}()

	//wg.Add(1)
	go func() {
		//defer wg.Done()

		for range trigger {
			for {
				var addr chunk.Address
				mu.Lock()
				for a := range addrs {
					b, err := hex.DecodeString(a)
					if err != nil {
						panic(err)
					}
					addr = chunk.Address(b)
					break
				}
				if err := s.Delete(addr); err != nil {
					panic(err)
				}
				delete(addrs, addr.String())
				if len(addrs) <= 900 {
					mu.Unlock()
					break
				}
				mu.Unlock()
			}
		}
	}()

	wg.Wait()

	// wait some time before removing the temp dir
	time.Sleep(time.Minute)
}

func dirSize(path string) (size int64, err error) {
	err = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".db") {
			size += info.Size()
		}
		return err
	})
	return size, err
}