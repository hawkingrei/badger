package badger

import (
	"bytes"
	"encoding/binary"
	"os"
	"sort"
	"sync/atomic"
	"unsafe"

	"github.com/ncw/directio"
	"github.com/pingcap/badger/fileutil"
	"github.com/pingcap/badger/protos"
	"github.com/pingcap/badger/table/memtable"
	"github.com/pingcap/badger/table/sstable"
	"github.com/pingcap/badger/y"
	"github.com/pingcap/log"
)

type shardingMemTables struct {
	tables []*memtable.CFTable // tables from new to old, the first one is mutable.
}

type engineTask struct {
	writeTask *WriteBatch
	splitTask *splitTask
}

type splitTask struct {
	shards []shardSplitTask
	notify chan error
}

type shardSplitTask struct {
	shard *Shard
	keys  [][]byte
}

func (sdb *ShardingDB) runWriteLoop(closer *y.Closer) {
	defer closer.Done()
	for {
		writeTasks, splitTask := sdb.collectTasks(closer)
		if len(writeTasks) == 0 && splitTask == nil {
			return
		}
		if len(writeTasks) > 0 {
			sdb.executeWriteTasks(writeTasks)
		}
		if splitTask != nil {
			sdb.executeSplitTask(splitTask)
		}
	}
}

func (sdb *ShardingDB) collectTasks(c *y.Closer) ([]*WriteBatch, *splitTask) {
	var writeTasks []*WriteBatch
	var splitTask *splitTask
	select {
	case x := <-sdb.writeCh:
		if x.writeTask != nil {
			writeTasks = append(writeTasks, x.writeTask)
		} else {
			splitTask = x.splitTask
		}
		l := len(sdb.writeCh)
		for i := 0; i < l; i++ {
			x = <-sdb.writeCh
			if x.writeTask != nil {
				writeTasks = append(writeTasks, x.writeTask)
			} else {
				// There is only one split tasks at a time.
				splitTask = x.splitTask
			}
		}
	case <-c.HasBeenClosed():
		return nil, nil
	}
	return writeTasks, splitTask
}

func (sdb *ShardingDB) loadWritableMemTable() *memtable.CFTable {
	tbls := sdb.loadMemTableSlice()
	return tbls.tables[0]
}

func (sdb *ShardingDB) loadMemTableSlice() *shardingMemTables {
	return (*shardingMemTables)(atomic.LoadPointer(&sdb.memTbls))
}

func (sdb *ShardingDB) switchMemTable(minSize int64) {
	writableMemTbl := sdb.loadWritableMemTable()
	newTableSize := sdb.opt.MaxMemTableSize
	if newTableSize < minSize {
		newTableSize = minSize
	}
	log.S().Infof("switch mem table new size %d", newTableSize)
	newMemTable := memtable.NewCFTable(newTableSize, sdb.opt.NumCFs)
	for {
		oldMemTbls := sdb.loadMemTableSlice()
		newMemTbls := &shardingMemTables{
			tables: make([]*memtable.CFTable, 0, len(oldMemTbls.tables)+1),
		}
		newMemTbls.tables = append(newMemTbls.tables, newMemTable)
		newMemTbls.tables = append(newMemTbls.tables, oldMemTbls.tables...)
		if atomic.CompareAndSwapPointer(&sdb.memTbls, unsafe.Pointer(oldMemTbls), unsafe.Pointer(newMemTbls)) {
			break
		}
	}
	if !writableMemTbl.Empty() {
		sdb.flushCh <- writableMemTbl
	}
}

func (sdb *ShardingDB) executeWriteTasks(tasks []*WriteBatch) {
	entries, estimatedSize := sdb.buildMemEntries(tasks)
	memTable := sdb.loadWritableMemTable()
	if memTable.Size()+estimatedSize > sdb.opt.MaxMemTableSize {
		sdb.switchMemTable(estimatedSize)
		memTable = sdb.loadWritableMemTable()
	}
	for cf, cfEntries := range entries {
		memTable.PutEntries(byte(cf), cfEntries)
	}
	for _, task := range tasks {
		task.notify <- nil
	}
}

func (sdb *ShardingDB) buildMemEntries(tasks []*WriteBatch) (entries [][]*memtable.Entry, estimateSize int64) {
	entries = make([][]*memtable.Entry, sdb.opt.NumCFs)
	for _, task := range tasks {
		for _, entry := range task.entries {
			memEntry := &memtable.Entry{Key: entry.key, Value: entry.val}
			entries[entry.cf] = append(entries[entry.cf], memEntry)
			estimateSize += memEntry.EstimateSize()
		}
	}
	for cf := 0; cf < sdb.opt.NumCFs; cf++ {
		cfEntries := entries[cf]
		sort.Slice(cfEntries, func(i, j int) bool {
			return bytes.Compare(cfEntries[i].Key, cfEntries[j].Key) < 0
		})
	}
	return
}

func (sdb *ShardingDB) createL0File(fid uint32) (fd, idxFD *os.File, err error) {
	filename := sstable.NewFilename(uint64(fid), sdb.opt.Dir)
	fd, err = directio.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, nil, err
	}
	idxFilename := sstable.IndexFilename(filename)
	idxFD, err = directio.OpenFile(idxFilename, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, nil, err
	}
	return fd, idxFD, nil
}

func (sdb *ShardingDB) runFlushMemTable(c *y.Closer) {
	defer c.Done()
	for m := range sdb.flushCh {
		fid := atomic.AddUint32(&sdb.lastFID, 1)
		fd, idxFD, err := sdb.createL0File(fid)
		if err != nil {
			panic(err)
		}
		err = sdb.flushMemTable(m, fd, idxFD)
		if err != nil {
			panic(err)
		}
		filename := fd.Name()
		fd.Close()
		idxFD.Close()
		l0Table, err := openShardL0Table(filename, fid)
		if err != nil {
			panic(err)
		}
		sdb.addL0Table(l0Table)
	}
}

func (sdb *ShardingDB) addL0Table(l0Table *shardL0Table) error {
	err := sdb.manifest.addChanges(&protos.HeadInfo{
		Version: sdb.orc.commitTs(),
	}, &protos.ManifestChange{
		Id:    uint64(l0Table.fid),
		Op:    protos.ManifestChange_CREATE,
		Level: 0,
	})
	if err != nil {
		return err
	}
	oldL0Tables := sdb.loadShardL0Tables()
	newL0Tables := &shardL0Tables{tables: make([]*shardL0Table, 0, len(oldL0Tables.tables)+1)}
	newL0Tables.tables = append(newL0Tables.tables, l0Table)
	newL0Tables.tables = append(newL0Tables.tables, oldL0Tables.tables...)
	atomic.StorePointer(&sdb.l0Tbls, unsafe.Pointer(newL0Tables))
	for {
		oldMemTbls := sdb.loadMemTableSlice()
		newMemTbls := &shardingMemTables{tables: make([]*memtable.CFTable, len(oldMemTbls.tables)-1)}
		copy(newMemTbls.tables, oldMemTbls.tables)
		if atomic.CompareAndSwapPointer(&sdb.memTbls, unsafe.Pointer(oldMemTbls), unsafe.Pointer(newMemTbls)) {
			break
		}
	}
	return nil
}

func (sdb *ShardingDB) flushMemTable(m *memtable.CFTable, fd, idxFD *os.File) error {
	writer := fileutil.NewDirectWriter(fd, sdb.opt.TableBuilderOptions.WriteBufferSize, nil)
	builders := map[uint32]*shardDataBuilder{}
	shardByKey := sdb.shards.loadShardByKeyTree()
	for cf := 0; cf < sdb.opt.NumCFs; cf++ {
		it := m.NewIterator(byte(cf))
		var lastShardBuilder *shardDataBuilder
		for it.SeekToFirst(); it.Valid(); it.Next() {
			var shardBuilder *shardDataBuilder
			if lastShardBuilder != nil && bytes.Compare(it.Key().UserKey, lastShardBuilder.shard.End) < 0 {
				shardBuilder = lastShardBuilder
			} else {
				shard := shardByKey.get(it.Key().UserKey)
				shardBuilder = builders[shard.ID]
				if shardBuilder == nil {
					shardBuilder = newShardDataBuilder(shard, sdb.opt.NumCFs, sdb.opt.TableBuilderOptions)
					builders[shard.ID] = shardBuilder
				}
				lastShardBuilder = shardBuilder
			}
			shardBuilder.Add(byte(cf), it.Key(), it.Value())
		}
	}
	shardDatas := make([][]byte, len(builders))
	shardIndex := &l0ShardIndex{
		startKeys:  make([][]byte, len(builders)),
		endOffsets: make([]uint32, len(builders)),
	}
	sortedBuilders := make([]*shardDataBuilder, 0, len(builders))
	for _, builder := range builders {
		sortedBuilders = append(sortedBuilders, builder)
	}
	sort.Slice(sortedBuilders, func(i, j int) bool {
		return bytes.Compare(sortedBuilders[i].shard.Start, sortedBuilders[j].shard.Start) < 0
	})
	endOffset := uint32(0)
	for i, builder := range sortedBuilders {
		shardData := builder.Finish()
		_, err := writer.Write(shardData)
		if err != nil {
			return err
		}
		shardDatas[i] = shardData
		endOffset += uint32(len(shardData))
		shardIndex.endOffsets[i] = endOffset
		shardIndex.startKeys[i] = builder.shard.Start
		if i == len(builders)-1 {
			shardIndex.endKey = sortedBuilders[len(sortedBuilders)-1].shard.End
		}
	}
	for _, shardData := range shardDatas {
		_, err := writer.Write(shardData)
		if err != nil {
			return err
		}
	}
	err := writer.Finish()
	if err != nil {
		return err
	}
	writer.Reset(idxFD)
	_, err = writer.Write(shardIndex.encode())
	if err != nil {
		return err
	}
	return writer.Finish()
}

func (sdb *ShardingDB) executeSplitTask(task *splitTask) {
	shardByKey := sdb.shards.loadShardByKeyTree()
	for _, shard := range task.shards {
		newShards := sdb.shards.split(shard.shard)
		shardByKey = shardByKey.replace([]*Shard{shard.shard}, newShards)
	}
	atomic.StorePointer(&sdb.shards.shardsByKey, unsafe.Pointer(shardByKey))
	task.notify <- nil
}

// l0 index file format
//  | numShards(4) | shardDataOffsets(4) ... | shardKeys(2 + len(key)) ...
type l0ShardIndex struct {
	startKeys  [][]byte
	endKey     []byte
	endOffsets []uint32
}

func (idx *l0ShardIndex) encode() []byte {
	l := 4 + 4 + 4 + len(idx.endOffsets)*4
	for _, key := range idx.startKeys {
		l += 2 + len(key)
	}
	l += 2 + len(idx.endKey)
	data := make([]byte, l)
	off := 0
	binary.LittleEndian.PutUint32(data[off:], uint32(len(idx.endOffsets)))
	off += 4
	for _, endOff := range idx.endOffsets {
		binary.LittleEndian.PutUint32(data[off:], endOff)
		off += 4
	}
	for _, startKey := range idx.startKeys {
		binary.LittleEndian.PutUint16(data[off:], uint16(len(startKey)))
		off += 2
		copy(data[off:], startKey)
		off += len(startKey)
	}
	binary.LittleEndian.PutUint16(data[off:], uint16(len(idx.endKey)))
	off += 2
	copy(data[off:], idx.endKey)
	return data
}

func (idx *l0ShardIndex) decode(data []byte) {
	off := 0
	numShard := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	idx.endOffsets = sstable.BytesToU32Slice(data[off : off+numShard*4])
	off += numShard * 4
	idx.startKeys = make([][]byte, numShard)
	for i := 0; i < numShard; i++ {
		keyLen := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		idx.startKeys[i] = data[off : off+keyLen]
		off += keyLen
	}
	endKeyLen := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	idx.endKey = data[off : off+endKeyLen]
}
