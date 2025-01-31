// Copyright (c) 2019 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package db

import (
	"bytes"
	"context"

	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"

	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/pkg/util/byteutil"
)

const fileMode = 0600

// boltDB is KVStore implementation based bolt DB
type boltDB struct {
	db          *bolt.DB
	path        string
	config      config.DB
	fillPercent map[string]float64 // specific fill percent for certain buckets (for example, 1.0 for append-only)
}

// NewBoltDB instantiates an BoltDB with implements KVStore
func NewBoltDB(cfg config.DB) KVStoreWithBucketFillPercent {
	return &boltDB{
		db:          nil,
		path:        cfg.DbPath,
		config:      cfg,
		fillPercent: make(map[string]float64),
	}
}

// Start opens the BoltDB (creates new file if not existing yet)
func (b *boltDB) Start(_ context.Context) error {
	db, err := bolt.Open(b.path, fileMode, nil)
	if err != nil {
		return errors.Wrap(ErrIO, err.Error())
	}
	b.db = db
	return nil
}

// Stop closes the BoltDB
func (b *boltDB) Stop(_ context.Context) error {
	if b.db != nil {
		if err := b.db.Close(); err != nil {
			return errors.Wrap(ErrIO, err.Error())
		}
	}
	return nil
}

// Put inserts a <key, value> record
func (b *boltDB) Put(namespace string, key, value []byte) (err error) {
	for c := uint8(0); c < b.config.NumRetries; c++ {
		if err = b.db.Update(func(tx *bolt.Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte(namespace))
			if err != nil {
				return err
			}
			if p, ok := b.fillPercent[namespace]; ok {
				bucket.FillPercent = p
			}
			return bucket.Put(key, value)
		}); err == nil {
			break
		}
	}
	if err != nil {
		err = errors.Wrap(ErrIO, err.Error())
	}
	return err
}

// Get retrieves a record
func (b *boltDB) Get(namespace string, key []byte) ([]byte, error) {
	var value []byte
	err := b.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(namespace))
		if bucket == nil {
			return errors.Wrapf(ErrNotExist, "bucket = %x doesn't exist", []byte(namespace))
		}
		v := bucket.Get(key)
		if v == nil {
			return errors.Wrapf(ErrNotExist, "key = %x doesn't exist", key)
		}
		value = make([]byte, len(v))
		// TODO: this is not an efficient way of passing the data
		copy(value, v)
		return nil
	})
	if err == nil {
		return value, nil
	}
	if errors.Cause(err) == ErrNotExist {
		return nil, err
	}
	return nil, errors.Wrap(ErrIO, err.Error())
}

// Range retrieves values for a range of keys
func (b *boltDB) Range(namespace string, key []byte, count uint64) ([][]byte, error) {
	value := make([][]byte, count)
	err := b.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(namespace))
		if bucket == nil {
			return errors.Wrapf(ErrNotExist, "bucket = %s doesn't exist", namespace)
		}
		// seek to start
		cur := bucket.Cursor()
		k, v := cur.Seek(key)
		if k == nil {
			return errors.Wrapf(ErrNotExist, "entry for key 0x%x doesn't exist", key)
		}
		// retrieve 'count' items
		for i := uint64(0); i < count; i++ {
			if k == nil {
				return errors.Wrapf(ErrNotExist, "entry for key 0x%x doesn't exist", k)
			}
			value[i] = make([]byte, len(v))
			copy(value[i], v)
			k, v = cur.Next()
		}
		return nil
	})
	if err == nil {
		return value, nil
	}
	if errors.Cause(err) == ErrNotExist {
		return nil, err
	}
	return nil, errors.Wrap(ErrIO, err.Error())
}

// GetBucketByPrefix retrieves all bucket those with const namespace prefix
func (b *boltDB) GetBucketByPrefix(namespace []byte) ([][]byte, error) {
	allKey := make([][]byte, 0)
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			if bytes.HasPrefix(name, namespace) && !bytes.Equal(name, namespace) {
				temp := make([]byte, len(name))
				copy(temp, name)
				allKey = append(allKey, temp)
			}
			return nil
		}); err != nil {
			return err
		}
		return nil
	})
	return allKey, err
}

// GetKeyByPrefix retrieves all keys those with const prefix
func (b *boltDB) GetKeyByPrefix(namespace, prefix []byte) ([][]byte, error) {
	allKey := make([][]byte, 0)
	err := b.db.View(func(tx *bolt.Tx) error {
		buck := tx.Bucket(namespace)
		if buck == nil {
			return ErrNotExist
		}
		c := buck.Cursor()
		for k, _ := c.Seek(prefix); bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			temp := make([]byte, len(k))
			copy(temp, k)
			allKey = append(allKey, temp)
		}
		return nil
	})
	return allKey, err
}

// Delete deletes a record,if key is nil,this will delete the whole bucket
func (b *boltDB) Delete(namespace string, key []byte) (err error) {
	numRetries := b.config.NumRetries
	for c := uint8(0); c < numRetries; c++ {
		if key == nil {
			err = b.db.Update(func(tx *bolt.Tx) error {
				if err := tx.DeleteBucket([]byte(namespace)); err != bolt.ErrBucketNotFound {
					return err
				}
				return nil
			})
		} else {
			err = b.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(namespace))
				if bucket == nil {
					return nil
				}
				return bucket.Delete(key)
			})
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		err = errors.Wrap(ErrIO, err.Error())
	}
	return err
}

// WriteBatch commits a batch
func (b *boltDB) WriteBatch(batch KVStoreBatch) (err error) {
	succeed := true
	batch.Lock()
	defer func() {
		if succeed {
			// clear the batch if commit succeeds
			batch.ClearAndUnlock()
		} else {
			batch.Unlock()
		}
	}()

	for c := uint8(0); c < b.config.NumRetries; c++ {
		if err = b.db.Update(func(tx *bolt.Tx) error {
			for i := 0; i < batch.Size(); i++ {
				write, err := batch.Entry(i)
				if err != nil {
					return err
				}
				if write.writeType == Put {
					bucket, err := tx.CreateBucketIfNotExists([]byte(write.namespace))
					if err != nil {
						return errors.Wrapf(err, write.errorFormat, write.errorArgs)
					}
					if p, ok := b.fillPercent[write.namespace]; ok {
						bucket.FillPercent = p
					}
					if err := bucket.Put(write.key, write.value); err != nil {
						return errors.Wrapf(err, write.errorFormat, write.errorArgs)
					}
				} else if write.writeType == Delete {
					bucket := tx.Bucket([]byte(write.namespace))
					if bucket == nil {
						continue
					}
					if err := bucket.Delete(write.key); err != nil {
						return errors.Wrapf(err, write.errorFormat, write.errorArgs)
					}
				}
			}
			return nil
		}); err == nil {
			break
		}
	}

	if err != nil {
		succeed = false
		err = errors.Wrap(ErrIO, err.Error())
	}
	return err
}

// SetBucketFillPercent sets specified fill percent for a bucket
func (b *boltDB) SetBucketFillPercent(namespace string, percent float64) error {
	b.fillPercent[namespace] = percent
	return nil
}

// ======================================
// below functions used by RangeIndex
// ======================================

// Insert inserts a value into the index
func (b *boltDB) Insert(name []byte, key uint64, value []byte) error {
	var err error
	for i := uint8(0); i < b.config.NumRetries; i++ {
		if err = b.db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(name)
			if bucket == nil {
				return errors.Wrapf(ErrBucketNotExist, "bucket = %x doesn't exist", name)
			}
			cur := bucket.Cursor()
			ak := byteutil.Uint64ToBytesBigEndian(key - 1)
			k, v := cur.Seek(ak)
			if !bytes.Equal(k, ak) {
				// insert new key
				if err := bucket.Put(ak, v); err != nil {
					return err
				}
			} else {
				// update an existing key
				k, _ = cur.Next()
			}
			if k != nil {
				return bucket.Put(k, value)
			}
			return nil
		}); err == nil {
			break
		}
	}
	if err != nil {
		err = errors.Wrap(ErrIO, err.Error())
	}
	return nil
}

// Seek returns value by the key
func (b *boltDB) Seek(name []byte, key uint64) ([]byte, error) {
	var value []byte
	err := b.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(name)
		if bucket == nil {
			return errors.Wrapf(ErrBucketNotExist, "bucket = %x doesn't exist", name)
		}
		// seek to start
		cur := bucket.Cursor()
		_, v := cur.Seek(byteutil.Uint64ToBytesBigEndian(key))
		value = make([]byte, len(v))
		copy(value, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return value, nil
}

// Remove removes an existing key
func (b *boltDB) Remove(name []byte, key uint64) error {
	var err error
	for i := uint8(0); i < b.config.NumRetries; i++ {
		if err = b.db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(name)
			if bucket == nil {
				return errors.Wrapf(ErrBucketNotExist, "bucket = %x doesn't exist", name)
			}
			cur := bucket.Cursor()
			ak := byteutil.Uint64ToBytesBigEndian(key - 1)
			k, v := cur.Seek(ak)
			if !bytes.Equal(k, ak) {
				// return nil if the key does not exist
				return nil
			}
			if err := bucket.Delete(ak); err != nil {
				return err
			}
			// write the corresponding value to next key
			k, _ = cur.Next()
			if k != nil {
				return bucket.Put(k, v)
			}
			return nil
		}); err == nil {
			break
		}
	}
	return err
}

// Purge deletes an existing key and all keys before it
func (b *boltDB) Purge(name []byte, key uint64) error {
	var err error
	for i := uint8(0); i < b.config.NumRetries; i++ {
		if err = b.db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(name)
			if bucket == nil {
				return errors.Wrapf(ErrBucketNotExist, "bucket = %x doesn't exist", name)
			}
			cur := bucket.Cursor()
			nk, _ := cur.Seek(byteutil.Uint64ToBytesBigEndian(key))
			// delete all keys before this key
			for k, _ := cur.Prev(); k != nil; k, _ = cur.Prev() {
				if err := bucket.Delete(k); err != nil {
					return err
				}
			}
			// write not exist value to next key
			if nk != nil {
				return bucket.Put(nk, NotExist)
			}
			return nil
		}); err == nil {
			break
		}
	}
	return err
}

// ======================================
// private functions
// ======================================

// intentionally fail to test DB can successfully rollback
func (b *boltDB) batchPutForceFail(namespace string, key [][]byte, value [][]byte) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(namespace))
		if err != nil {
			return err
		}
		if len(key) != len(value) {
			return errors.Wrap(ErrIO, "batch put <k, v> size not match")
		}
		for i := 0; i < len(key); i++ {
			if err := bucket.Put(key[i], value[i]); err != nil {
				return err
			}
			// intentionally fail to test DB can successfully rollback
			if i == len(key)-1 {
				return errors.Wrapf(ErrIO, "force fail to test DB rollback")
			}
		}
		return nil
	})
}
