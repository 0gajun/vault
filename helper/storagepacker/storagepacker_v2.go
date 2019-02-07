package storagepacker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	radix "github.com/armon/go-radix"
	"github.com/golang/protobuf/proto"
	any "github.com/golang/protobuf/ptypes/any"
	"github.com/hashicorp/errwrap"
	log "github.com/hashicorp/go-hclog"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/vault/helper/compressutil"
	"github.com/hashicorp/vault/helper/cryptoutil"
	"github.com/hashicorp/vault/helper/locksutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/physical"
)

const (
	defaultBaseBucketBits  = 8
	defaultBucketShardBits = 4
)

type Config struct {
	// BucketStorageView is the storage to be used by all the buckets
	BucketStorageView *logical.StorageView `json:"-"`

	// ConfigStorageView is the storage to store config info
	ConfigStorageView *logical.StorageView `json:"-"`

	// Logger for output
	Logger log.Logger `json:"-"`

	// BaseBucketBits is the number of bits to use for buckets at the base level
	BaseBucketBits int `json:"base_bucket_bits"`

	// BucketShardBits is the number of bits to use for sub-buckets a bucket
	// gets sharded into when it reaches the maximum threshold.
	BucketShardBits int `json:"-"`
}

// StoragePacker packs many items into abstractions called buckets. The goal
// is to employ a reduced number of storage entries for a relatively huge
// number of items. This is the second version of the utility which supports
// indefinitely expanding the capacity of the storage by sharding the buckets
// when they exceed the imposed limit.
type StoragePackerV2 struct {
	*Config
	storageLocks []*locksutil.LockEntry
	bucketsCache *radix.Tree

	// Note that we're slightly loosy-goosy with this lock. The reason is that
	// outside of an identity store upgrade case, only PutItem will ever write
	// a bucket, and that will always fetch a lock on the bucket first. This
	// will also cover the sharding case since you'd get the parent lock first.
	// So we can get away with only locking just when modifying, because we
	// should already be locked in terms of an entry overwriting itself.
	bucketsCacheLock sync.RWMutex

	queueMode     uint32
	queuedBuckets sync.Map
}

// LockedBucket embeds a bucket and its corresponding lock to ensure thread
// safety
type LockedBucket struct {
	sync.RWMutex
	*Bucket
}

func (s *StoragePackerV2) BucketsView() *logical.StorageView {
	return s.BucketStorageView
}

func (s *StoragePackerV2) BucketStorageKeyForItemID(itemID string) string {
	hexVal := hex.EncodeToString(cryptoutil.Blake2b256Hash(itemID))

	s.bucketsCacheLock.RLock()
	_, bucketRaw, found := s.bucketsCache.LongestPrefix(hexVal)
	s.bucketsCacheLock.RUnlock()

	if found {
		return bucketRaw.(*LockedBucket).Key
	}

	// If we have existing buckets we'd have parsed them in on startup
	// (assuming that all users load all entries on startup), so this is a
	// fresh storagepacker, so we use the root bits to return a proper number
	// of chars. But first do that, lock, and try again to ensure nothing
	// changed without holding a lock.
	cacheKey := hexVal[0 : s.BaseBucketBits/4]
	lock := locksutil.LockForKey(s.storageLocks, cacheKey)
	lock.RLock()

	s.bucketsCacheLock.RLock()
	_, bucketRaw, found = s.bucketsCache.LongestPrefix(hexVal)
	s.bucketsCacheLock.RUnlock()

	lock.RUnlock()

	if found {
		return bucketRaw.(*LockedBucket).Key
	}

	return cacheKey
}

func (s *StoragePackerV2) BucketKeyHashByItemID(itemID string) string {
	return s.GetCacheKey(s.BucketStorageKeyForItemID(itemID))
}

func (s *StoragePackerV2) GetCacheKey(key string) string {
	return strings.Replace(key, "/", "", -1)
}

func (s *StoragePackerV2) BucketKeys(ctx context.Context) ([]string, error) {
	keys := map[string]struct{}{}
	diskBuckets, err := logical.CollectKeys(ctx, s.BucketStorageView)
	if err != nil {
		return nil, err
	}
	for _, bucket := range diskBuckets {
		keys[bucket] = struct{}{}
	}

	s.bucketsCacheLock.RLock()
	s.bucketsCache.Walk(func(s string, _ interface{}) bool {
		keys[s] = struct{}{}
		return false
	})
	s.bucketsCacheLock.RUnlock()

	ret := make([]string, 0, len(keys))
	for k := range keys {
		ret = append(ret, k)
	}

	return ret, nil
}

// Get returns a bucket for a given key
func (s *StoragePackerV2) GetBucket(ctx context.Context, key string, skipCache bool) (*LockedBucket, error) {
	cacheKey := s.GetCacheKey(key)

	if key == "" {
		return nil, fmt.Errorf("missing bucket key")
	}

	lock := locksutil.LockForKey(s.storageLocks, cacheKey)
	lock.RLock()

	s.bucketsCacheLock.RLock()
	_, bucketRaw, found := s.bucketsCache.LongestPrefix(cacheKey)
	s.bucketsCacheLock.RUnlock()

	if found && !skipCache {
		ret := bucketRaw.(*LockedBucket)
		lock.RUnlock()
		return ret, nil
	}

	// Swap out for a write lock
	lock.RUnlock()
	lock.Lock()
	defer lock.Unlock()

	// Check for it to have been added
	s.bucketsCacheLock.RLock()
	_, bucketRaw, found = s.bucketsCache.LongestPrefix(cacheKey)
	s.bucketsCacheLock.RUnlock()

	if found && !skipCache {
		ret := bucketRaw.(*LockedBucket)
		return ret, nil
	}

	// Read from the underlying view
	storageEntry, err := s.BucketStorageView.Get(ctx, key)
	if err != nil {
		return nil, errwrap.Wrapf("failed to read packed storage entry: {{err}}", err)
	}
	if storageEntry == nil {
		return nil, nil
	}

	bucket, err := s.DecodeBucket(storageEntry)
	if err != nil {
		return nil, err
	}

	s.bucketsCacheLock.Lock()
	s.bucketsCache.Insert(cacheKey, bucket)
	s.bucketsCacheLock.Unlock()

	return bucket, nil
}

// NOTE: Don't put inserting into the cache here, as that will mess with
// upgrade cases for the identity store as we want to keep the bucket out of
// the cache until we actually re-store it.
func (s *StoragePackerV2) DecodeBucket(storageEntry *logical.StorageEntry) (*LockedBucket, error) {
	uncompressedData, notCompressed, err := compressutil.Decompress(storageEntry.Value)
	if err != nil {
		return nil, errwrap.Wrapf("failed to decompress packed storage entry: {{err}}", err)
	}
	if notCompressed {
		uncompressedData = storageEntry.Value
	}

	var bucket Bucket
	err = proto.Unmarshal(uncompressedData, &bucket)
	if err != nil {
		return nil, errwrap.Wrapf("failed to decode packed storage entry: {{err}}", err)
	}

	lb := &LockedBucket{
		Bucket: &bucket,
	}
	lb.Key = storageEntry.Key

	return lb, nil
}

// Put stores a packed bucket entry
func (s *StoragePackerV2) PutBucket(ctx context.Context, bucket *LockedBucket) error {
	if bucket == nil {
		return fmt.Errorf("nil bucket entry")
	}

	if bucket.Key == "" {
		return fmt.Errorf("missing key")
	}

	cacheKey := s.GetCacheKey(bucket.Key)

	lock := locksutil.LockForKey(s.storageLocks, cacheKey)
	lock.Lock()
	defer lock.Unlock()

	bucket.Lock()
	defer bucket.Unlock()

	if err := s.storeBucket(ctx, bucket); err != nil {
		if strings.Contains(err.Error(), physical.ErrValueTooLarge) {
			err = s.shardBucket(ctx, bucket)
		}
		if err != nil {
			return err
		}
	}

	s.bucketsCacheLock.Lock()
	s.bucketsCache.Insert(s.GetCacheKey(bucket.Key), bucket)
	s.bucketsCacheLock.Unlock()

	return nil
}

func (s *StoragePacker) shardBucket(ctx context.Context, bucket *LockedBucket) error {
	for i := 0; i < 2^s.BucketShardBits; i++ {
		shardedBucket := &LockedBucket{Bucket: &Bucket{}}
		bucket.Buckets[fmt.Sprintf("%x", i)] = shardedBucket
	}
	cacheKey := hexVal[0 : s.BaseBucketBits/4]
	lock := locksutil.LockForKey(s.storageLocks, cacheKey)
	lock.RLock()

}

// storeBucket actually stores the bucket. It expects that it's already locked.
func (s *StoragePackerV2) storeBucket(ctx context.Context, bucket *LockedBucket) error {
	if atomic.LoadUint32(&s.queueMode) == 1 {
		s.queuedBuckets.Store(bucket.Key, bucket)
		return nil
	}

	marshaledBucket, err := proto.Marshal(bucket.Bucket)
	if err != nil {
		return errwrap.Wrapf("failed to marshal bucket: {{err}}", err)
	}

	compressedBucket, err := compressutil.Compress(marshaledBucket, &compressutil.CompressionConfig{
		Type: compressutil.CompressionTypeSnappy,
	})
	if err != nil {
		return errwrap.Wrapf("failed to compress packed bucket: {{err}}", err)
	}

	// Store the compressed value
	err = s.BucketStorageView.Put(ctx, &logical.StorageEntry{
		Key:   bucket.Key,
		Value: compressedBucket,
	})
	if err != nil {
		return errwrap.Wrapf("failed to persist packed storage entry: {{err}}", err)
	}

	return nil
}

// DeleteBucket deletes an entire bucket entry
func (s *StoragePackerV2) DeleteBucket(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("missing key")
	}

	cacheKey := s.GetCacheKey(key)

	lock := locksutil.LockForKey(s.storageLocks, cacheKey)
	lock.Lock()
	defer lock.Unlock()

	if err := s.BucketStorageView.Delete(ctx, key); err != nil {
		return errwrap.Wrapf("failed to delete packed storage entry: {{err}}", err)
	}

	s.bucketsCacheLock.Lock()
	s.bucketsCache.Delete(cacheKey)
	s.bucketsCacheLock.Unlock()

	return nil
}

// upsert either inserts a new item into the bucket or updates an existing one
// if an item with a matching key is already present.
func (s *LockedBucket) upsert(item *Item) error {
	if s == nil {
		return fmt.Errorf("nil storage bucket")
	}

	if item == nil {
		return fmt.Errorf("nil item")
	}

	if item.ID == "" {
		return fmt.Errorf("missing item ID")
	}

	if s.ItemMap == nil {
		s.ItemMap = make(map[string]*any.Any)
	}

	s.ItemMap[item.ID] = item.Message
	return nil
}

// DeleteItem removes the storage entry which the given key refers to from its
// corresponding bucket.
func (s *StoragePackerV2) DeleteItem(ctx context.Context, itemID string) error {
	if itemID == "" {
		return fmt.Errorf("empty item ID")
	}

	// Get the bucket key
	bucketKey := s.BucketStorageKeyForItemID(itemID)
	cacheKey := s.GetCacheKey(bucketKey)

	lock := locksutil.LockForKey(s.storageLocks, cacheKey)
	lock.Lock()
	defer lock.Unlock()

	var bucket *LockedBucket

	s.bucketsCacheLock.RLock()
	_, bucketRaw, found := s.bucketsCache.LongestPrefix(cacheKey)
	s.bucketsCacheLock.RUnlock()

	if found {
		bucket = bucketRaw.(*LockedBucket)
	} else {
		// Read from underlying view
		storageEntry, err := s.BucketStorageView.Get(ctx, bucketKey)
		if err != nil {
			return errwrap.Wrapf("failed to read packed storage value: {{err}}", err)
		}
		if storageEntry == nil {
			return nil
		}

		bucket, err = s.DecodeBucket(storageEntry)
		if err != nil {
			return errwrap.Wrapf("error decoding existing storage entry for upsert: {{err}}", err)
		}

		s.bucketsCacheLock.Lock()
		s.bucketsCache.Insert(cacheKey, bucket)
		s.bucketsCacheLock.Unlock()
	}

	bucket.Lock()
	defer bucket.Unlock()

	if len(bucket.ItemMap) == 0 {
		return nil
	}

	_, ok := bucket.ItemMap[itemID]
	if !ok {
		return nil
	}

	delete(bucket.ItemMap, itemID)
	return s.storeBucket(ctx, bucket)
}

// GetItem fetches the storage entry for a given key from its corresponding
// bucket.
func (s *StoragePackerV2) GetItem(ctx context.Context, itemID string) (*Item, error) {
	if itemID == "" {
		return nil, fmt.Errorf("empty item ID")
	}

	bucketKey := s.BucketStorageKeyForItemID(itemID)
	cacheKey := s.GetCacheKey(bucketKey)

	lock := locksutil.LockForKey(s.storageLocks, cacheKey)
	lock.RLock()
	defer lock.RUnlock()

	var bucket *LockedBucket

	s.bucketsCacheLock.RLock()
	_, bucketRaw, found := s.bucketsCache.LongestPrefix(cacheKey)
	s.bucketsCacheLock.RUnlock()

	if found {
		bucket = bucketRaw.(*LockedBucket)
	} else {
		// Read from underlying view
		storageEntry, err := s.BucketStorageView.Get(ctx, bucketKey)
		if err != nil {
			return nil, errwrap.Wrapf("failed to read packed storage value: {{err}}", err)
		}
		if storageEntry == nil {
			return nil, nil
		}

		bucket, err = s.DecodeBucket(storageEntry)
		if err != nil {
			return nil, errwrap.Wrapf("error decoding existing storage entry for upsert: {{err}}", err)
		}

		s.bucketsCacheLock.Lock()
		s.bucketsCache.Insert(cacheKey, bucket)
		s.bucketsCacheLock.Unlock()
	}

	bucket.RLock()

	if len(bucket.ItemMap) == 0 {
		bucket.RUnlock()
		return nil, nil
	}

	item, ok := bucket.ItemMap[itemID]
	if !ok {
		bucket.RUnlock()
		return nil, nil
	}

	bucket.RUnlock()
	return &Item{
		ID:      itemID,
		Message: item,
	}, nil
}

// PutItem stores a storage entry in its corresponding bucket
func (s *StoragePackerV2) PutItem(ctx context.Context, item *Item) error {
	if item == nil {
		return fmt.Errorf("nil item")
	}

	if item.ID == "" {
		return fmt.Errorf("missing ID in item")
	}

	// Get the bucket key
	bucketKey := s.BucketStorageKeyForItemID(item.ID)
	cacheKey := s.GetCacheKey(bucketKey)

	lock := locksutil.LockForKey(s.storageLocks, cacheKey)
	lock.Lock()
	defer lock.Unlock()

	var bucket *LockedBucket

	s.bucketsCacheLock.RLock()
	_, bucketRaw, found := s.bucketsCache.LongestPrefix(cacheKey)
	s.bucketsCacheLock.RUnlock()

	if found {
		bucket = bucketRaw.(*LockedBucket)
	} else {
		// Read from underlying view
		storageEntry, err := s.BucketStorageView.Get(ctx, bucketKey)
		if err != nil {
			return errwrap.Wrapf("failed to read packed storage value: {{err}}", err)
		}

		if storageEntry == nil {
			bucket = &LockedBucket{
				Bucket: &Bucket{
					Key: bucketKey,
				},
			}
		} else {
			bucket, err = s.DecodeBucket(storageEntry)
			if err != nil {
				return errwrap.Wrapf("error decoding existing storage entry for upsert: {{err}}", err)
			}
		}

		s.bucketsCacheLock.Lock()
		s.bucketsCache.Insert(cacheKey, bucket)
		s.bucketsCacheLock.Unlock()
	}

	bucket.Lock()
	defer bucket.Unlock()

	if err := bucket.upsert(item); err != nil {
		return errwrap.Wrapf("failed to update entry in packed storage entry: {{err}}", err)
	}

	// Persist the result
	return s.storeBucket(ctx, bucket)
}

// NewStoragePackerV2 creates a new storage packer for a given view
func NewStoragePackerV2(ctx context.Context, config *Config) (StoragePacker, error) {
	if config.BucketStorageView == nil {
		return nil, fmt.Errorf("nil buckets view")
	}

	config.BucketStorageView = config.BucketStorageView.SubView("v2/")

	if config.ConfigStorageView == nil {
		return nil, fmt.Errorf("nil config view")
	}

	if config.BaseBucketBits == 0 {
		config.BaseBucketBits = defaultBaseBucketBits
	}

	// At this point, look for an existing saved configuration
	var needPersist bool
	entry, err := config.ConfigStorageView.Get(ctx, "config")
	if err != nil {
		return nil, errwrap.Wrapf("error checking for existing storagepacker config: {{err}}", err)
	}
	if entry != nil {
		needPersist = false
		var exist Config
		if err := entry.DecodeJSON(&exist); err != nil {
			return nil, errwrap.Wrapf("error decoding existing storagepacker config: {{err}}", err)
		}
		// If we have an existing config, we copy the only thing we need
		// constant: the bucket base count, so we know how many to expect at
		// the base level
		//
		// The rest of the values can change
		config.BaseBucketBits = exist.BaseBucketBits
	}

	if config.BucketShardBits == 0 {
		config.BucketShardBits = defaultBucketShardBits
	}

	if config.BaseBucketBits%4 != 0 {
		return nil, fmt.Errorf("bucket base bits of %d is not a multiple of four", config.BaseBucketBits)
	}

	if config.BucketShardBits%4 != 0 {
		return nil, fmt.Errorf("bucket shard count of %d is not a power of two", config.BucketShardBits)
	}

	if config.BaseBucketBits < 4 {
		return nil, errors.New("bucket base bits should be at least 4")
	}
	if config.BucketShardBits < 4 {
		return nil, errors.New("bucket shard count should at least be 4")
	}

	if needPersist {
		entry, err := logical.StorageEntryJSON("config", config)
		if err != nil {
			return nil, errwrap.Wrapf("error encoding storagepacker config: {{err}}", err)
		}
		if err := config.ConfigStorageView.Put(ctx, entry); err != nil {
			return nil, errwrap.Wrapf("error storing storagepacker config: {{err}}", err)
		}
	}

	// Create a new packer object for the given view
	packer := &StoragePackerV2{
		Config:       config,
		bucketsCache: radix.New(),
		storageLocks: locksutil.CreateLocks(),
	}

	return packer, nil
}

func (s *StoragePackerV2) SetQueueMode(enabled bool) {
	if enabled {
		atomic.StoreUint32(&s.queueMode, 1)
	} else {
		atomic.StoreUint32(&s.queueMode, 0)
	}
}

func (s *StoragePackerV2) FlushQueue(ctx context.Context) error {
	var err *multierror.Error
	s.queuedBuckets.Range(func(key, value interface{}) bool {
		lErr := s.storeBucket(ctx, value.(*LockedBucket))
		if lErr != nil {
			err = multierror.Append(err, lErr)
		}
		s.queuedBuckets.Delete(key)
		return true
	})

	return err.ErrorOrNil()
}
