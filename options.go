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
	"github.com/pingcap/badger/options"
	"github.com/pingcap/badger/s3util"
)

// NOTE: Keep the comments in the following to 75 chars width, so they
// format nicely in godoc.

// Options are params for creating DB object.
//
// This package provides DefaultOptions which contains options that should
// work for most applications. Consider using that as a starting point before
// customizing it for your own needs.
type Options struct {
	// 1. Mandatory flags
	// -------------------
	// Directory to store the data in. Should exist and be writable.
	Dir string
	// Directory to store the value log in. Can be the same as Dir. Should
	// exist and be writable.
	ValueDir string

	// 2. Frequently modified flags
	// -----------------------------
	// Sync all writes to disk. Setting this to true would slow down data
	// loading significantly.
	SyncWrites bool

	// 3. Flags that user might want to review
	// ----------------------------------------
	// The following affect all levels of LSM tree.
	MaxMemTableSize int64 // Each mem table is at most this size.
	// If value size >= this threshold, only store value offsets in tree.
	// If set to 0, all values are stored in SST.
	ValueThreshold int
	// Maximum number of tables to keep in memory, before stalling.
	NumMemtables int
	// The following affect how we handle LSM tree L0.
	// Maximum number of Level 0 tables before we start compacting.
	NumLevelZeroTables int

	// If we hit this number of Level 0 tables, we will stall until L0 is
	// compacted away.
	NumLevelZeroTablesStall int

	MaxBlockCacheSize int64
	MaxIndexCacheSize int64

	// Maximum total size for L1.
	LevelOneSize int64

	// Size of single value log file.
	ValueLogFileSize int64

	// Max number of entries a value log file can hold (approximately). A value log file would be
	// determined by the smaller of its file size and max entries.
	ValueLogMaxEntries uint32

	// Max number of value log files to keep before safely remove.
	ValueLogMaxNumFiles int

	// Number of compaction workers to run concurrently.
	NumCompactors int

	// Transaction start and commit timestamps are managed by end-user.
	// A managed transaction can only set values by SetEntry with a non-zero version key.
	ManagedTxns bool

	// 4. Flags for testing purposes
	// ------------------------------
	VolatileMode bool
	DoNotCompact bool // Stops LSM tree from compactions.

	maxBatchCount int64 // max entries in batch
	maxBatchSize  int64 // max batch size in bytes

	// Open the DB as read-only. With this set, multiple processes can
	// open the same Badger DB. Note: if the DB being opened had crashed
	// before and has vlog data to be replayed, ReadOnly will cause Open
	// to fail with an appropriate message.
	ReadOnly bool

	// Truncate value log to delete corrupt data, if any. Would not truncate if ReadOnly is set.
	Truncate bool

	TableBuilderOptions options.TableBuilderOptions

	ValueLogWriteOptions options.ValueLogWriterOptions

	CompactionFilterFactory func(targetLevel int, smallest, biggest []byte) CompactionFilter

	CompactL0WhenClose bool

	RemoteCompactionAddr string

	S3Options s3util.Options

	CFs []CFConfig

	IDAllocator IDAllocator
}

type CFConfig struct {
	Managed bool
}

// CompactionFilter is an interface that user can implement to remove certain keys.
type CompactionFilter interface {
	// Filter is the method the compaction process invokes for kv that is being compacted. The returned decision
	// indicates that the kv should be preserved, deleted or dropped in the output of this compaction run.
	Filter(key, val, userMeta []byte) Decision

	// Guards returns specifications that may splits the SST files
	// A key is associated to a guard that has the longest matched Prefix.
	Guards() []Guard
}

// Guard specifies when to finish a SST file during compaction. The rule is the following:
// 1. The key must match the Prefix of the Guard, otherwise the SST should finish.
// 2. If the key up to MatchLen is the different than the previous key and MinSize is reached, the SST should finish.
type Guard struct {
	Prefix   []byte
	MatchLen int
	MinSize  int64
}

// Decision is the type for compaction filter decision.
type Decision int

const (
	// DecisionKeep indicates the entry should be reserved.
	DecisionKeep Decision = 0
	// DecisionMarkTombstone converts the entry to a delete tombstone.
	DecisionMarkTombstone Decision = 1
	// DecisionDrop simply drops the entry, doesn't leave a delete tombstone.
	DecisionDrop Decision = 2
)

// IDAllocator is a function that allocated file ID.
type IDAllocator interface {
	AllocID() uint64
}

// DefaultOptions sets a list of recommended options for good performance.
// Feel free to modify these to suit your needs.
var DefaultOptions = Options{
	DoNotCompact:            false,
	LevelOneSize:            256 << 20,
	MaxMemTableSize:         64 << 20,
	NumCompactors:           3,
	NumLevelZeroTables:      5,
	NumLevelZeroTablesStall: 10,
	NumMemtables:            5,
	SyncWrites:              false,
	ValueLogFileSize:        256 << 20,
	ValueLogMaxEntries:      1000000,
	ValueLogMaxNumFiles:     1,
	ValueThreshold:          0,
	Truncate:                false,
	MaxBlockCacheSize:       0,
	MaxIndexCacheSize:       0,
	TableBuilderOptions: options.TableBuilderOptions{
		MaxTableSize:        8 << 20,
		SuRFStartLevel:      8,
		HashUtilRatio:       0.75,
		WriteBufferSize:     2 * 1024 * 1024,
		BytesPerSecond:      -1,
		MaxLevels:           7,
		LevelSizeMultiplier: 10,
		BlockSize:           64 * 1024,
		LogicalBloomFPR:     0.01,
		SuRFOptions: options.SuRFOptions{
			HashSuffixLen:  8,
			RealSuffixLen:  8,
			BitsPerKeyHint: 40,
		},
	},
	ValueLogWriteOptions: options.ValueLogWriterOptions{
		WriteBufferSize: 2 * 1024 * 1024,
	},
	CompactL0WhenClose: true,
}

// LSMOnlyOptions follows from DefaultOptions, but sets a higher ValueThreshold so values would
// be colocated with the LSM tree, with value log largely acting as a write-ahead log only. These
// options would reduce the disk usage of value log, and make Badger act like a typical LSM tree.
var LSMOnlyOptions = Options{}

func init() {
	LSMOnlyOptions = DefaultOptions

	LSMOnlyOptions.ValueThreshold = 65500      // Max value length which fits in uint16.
	LSMOnlyOptions.ValueLogFileSize = 64 << 20 // Allow easy space reclamation.
}
