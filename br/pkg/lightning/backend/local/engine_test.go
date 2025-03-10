// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package local

import (
	"context"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/google/uuid"
	"github.com/pingcap/tidb/br/pkg/lightning/backend"
	"github.com/pingcap/tidb/br/pkg/lightning/log"
	"github.com/stretchr/testify/require"
)

func makePebbleDB(t *testing.T, opt *pebble.Options) (*pebble.DB, string) {
	dir := t.TempDir()
	db, err := pebble.Open(path.Join(dir, "test"), opt)
	require.NoError(t, err)
	tmpPath := filepath.Join(dir, "test.sst")
	err = os.Mkdir(tmpPath, 0o755)
	require.NoError(t, err)
	return db, tmpPath
}

func TestGetEngineSizeWhenImport(t *testing.T) {
	opt := &pebble.Options{
		MemTableSize:             1024 * 1024,
		MaxConcurrentCompactions: 16,
		L0CompactionThreshold:    math.MaxInt32, // set to max try to disable compaction
		L0StopWritesThreshold:    math.MaxInt32, // set to max try to disable compaction
		DisableWAL:               true,
		ReadOnly:                 false,
	}
	db, tmpPath := makePebbleDB(t, opt)

	_, engineUUID := backend.MakeUUID("ww", 0)
	engineCtx, cancel := context.WithCancel(context.Background())
	f := &Engine{
		UUID:         engineUUID,
		sstDir:       tmpPath,
		ctx:          engineCtx,
		cancel:       cancel,
		sstMetasChan: make(chan metaOrFlush, 64),
		keyAdapter:   noopKeyAdapter{},
		logger:       log.L(),
	}
	f.db.Store(db)
	// simulate import
	f.lock(importMutexStateImport)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		engineFileSize := f.getEngineFileSize()
		require.Equal(t, f.UUID, engineFileSize.UUID)
		require.True(t, engineFileSize.IsImporting)
	}()
	wg.Wait()
	f.unlock()
	require.NoError(t, f.Close())
}

func TestIngestSSTWithClosedEngine(t *testing.T) {
	opt := &pebble.Options{
		MemTableSize:             1024 * 1024,
		MaxConcurrentCompactions: 16,
		L0CompactionThreshold:    math.MaxInt32, // set to max try to disable compaction
		L0StopWritesThreshold:    math.MaxInt32, // set to max try to disable compaction
		DisableWAL:               true,
		ReadOnly:                 false,
	}
	db, tmpPath := makePebbleDB(t, opt)

	_, engineUUID := backend.MakeUUID("ww", 0)
	engineCtx, cancel := context.WithCancel(context.Background())
	f := &Engine{
		UUID:         engineUUID,
		sstDir:       tmpPath,
		ctx:          engineCtx,
		cancel:       cancel,
		sstMetasChan: make(chan metaOrFlush, 64),
		keyAdapter:   noopKeyAdapter{},
		logger:       log.L(),
	}
	f.db.Store(db)
	f.sstIngester = dbSSTIngester{e: f}
	sstPath := path.Join(tmpPath, uuid.New().String()+".sst")
	file, err := os.Create(sstPath)
	require.NoError(t, err)
	w := sstable.NewWriter(file, sstable.WriterOptions{})
	for i := 0; i < 10; i++ {
		require.NoError(t, w.Add(sstable.InternalKey{
			Trailer: uint64(sstable.InternalKeyKindSet),
			UserKey: []byte(fmt.Sprintf("key%d", i)),
		}, nil))
	}
	require.NoError(t, w.Close())

	require.NoError(t, f.ingestSSTs([]*sstMeta{
		{
			path: sstPath,
		},
	}))
	require.NoError(t, f.Close())
	require.ErrorIs(t, f.ingestSSTs([]*sstMeta{
		{
			path: sstPath,
		},
	}), errorEngineClosed)
}

func TestGetFirstAndLastKey(t *testing.T) {
	db, tmpPath := makePebbleDB(t, nil)
	f := &Engine{
		sstDir: tmpPath,
	}
	f.db.Store(db)
	err := db.Set([]byte("a"), []byte("a"), nil)
	require.NoError(t, err)
	err = db.Set([]byte("c"), []byte("c"), nil)
	require.NoError(t, err)
	err = db.Set([]byte("e"), []byte("e"), nil)
	require.NoError(t, err)

	first, last, err := f.getFirstAndLastKey(nil, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("a"), first)
	require.Equal(t, []byte("e"), last)

	first, last, err = f.getFirstAndLastKey([]byte("b"), []byte("d"))
	require.NoError(t, err)
	require.Equal(t, []byte("c"), first)
	require.Equal(t, []byte("c"), last)

	first, last, err = f.getFirstAndLastKey([]byte("b"), []byte("f"))
	require.NoError(t, err)
	require.Equal(t, []byte("c"), first)
	require.Equal(t, []byte("e"), last)

	first, last, err = f.getFirstAndLastKey([]byte("y"), []byte("z"))
	require.NoError(t, err)
	require.Nil(t, first)
	require.Nil(t, last)

	first, last, err = f.getFirstAndLastKey([]byte("e"), []byte(""))
	require.NoError(t, err)
	require.Equal(t, []byte("e"), first)
	require.Equal(t, []byte("e"), last)
}
