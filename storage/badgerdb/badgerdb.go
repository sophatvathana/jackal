/*
 * Copyright (c) 2018 Miguel Ángel Ortuño.
 * See the LICENSE file for more information.
 */

package badgerdb

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/ortuman/jackal/log"
	"github.com/ortuman/jackal/pool"
	"github.com/ortuman/jackal/storage/model"
)

var (
	errBadgerDBWrongEntityType = errors.New("badgerdb: wrong entity type")
	errBadgerDBEntityNotFound  = errors.New("badgerdb: entity not found")
)

// Config represents BadgerDB storage configuration.
type Config struct {
	DataDir string `yaml:"data_dir"`
}

// Storage represents a BadgerDB storage sub system.
type Storage struct {
	db     *badger.DB
	pool   *pool.BufferPool
	doneCh chan chan bool
}

// New returns a new BadgerDB storage instance.
func New(cfg *Config) *Storage {
	b := &Storage{
		pool:   pool.NewBufferPool(),
		doneCh: make(chan chan bool),
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DataDir), os.ModePerm); err != nil {
		log.Fatalf("%v", err)
	}
	opts := badger.DefaultOptions
	opts.Dir = cfg.DataDir
	opts.ValueDir = cfg.DataDir
	db, err := badger.Open(opts)
	if err != nil {
		log.Fatalf("%v", err)
	}
	b.db = db
	go b.loop()
	return b
}

// Shutdown shuts down BadgerDB storage sub system.
func (b *Storage) Shutdown() {
	ch := make(chan bool)
	b.doneCh <- ch
	<-ch
}

func (b *Storage) loop() {
	tc := time.NewTicker(time.Minute)
	defer tc.Stop()
	for {
		select {
		case <-tc.C:
			b.db.PurgeOlderVersions()
			b.db.RunValueLogGC(0.5)
		case ch := <-b.doneCh:
			b.db.Close()
			close(ch)
			return
		}
	}
}

func (b *Storage) insertOrUpdate(entity interface{}, key []byte, tx *badger.Txn) error {
	gs, ok := entity.(model.GobSerializer)
	if !ok {
		return fmt.Errorf("%v: %T", errBadgerDBWrongEntityType, entity)
	}
	buf := b.pool.Get()
	defer b.pool.Put(buf)

	gs.ToGob(gob.NewEncoder(buf))
	bts := buf.Bytes()
	val := make([]byte, len(bts))
	copy(val, bts)
	return tx.Set(key, val)
}

func (b *Storage) delete(key []byte, txn *badger.Txn) error {
	return txn.Delete(key)
}

func (b *Storage) deletePrefix(prefix []byte, txn *badger.Txn) error {
	var keys [][]byte
	if err := b.forEachKey(prefix, func(key []byte) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		return err
	}
	for _, k := range keys {
		if err := txn.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

func (b *Storage) fetch(entity interface{}, key []byte) error {
	return b.db.View(func(tx *badger.Txn) error {
		val, err := b.getVal(key, tx)
		if err != nil {
			return err
		}
		if val != nil {
			if entity != nil {
				gd, ok := entity.(model.GobDeserializer)
				if !ok {
					return fmt.Errorf("%v: %T", errBadgerDBWrongEntityType, entity)
				}
				gd.FromGob(gob.NewDecoder(bytes.NewReader(val)))
			}
			return nil
		}
		return errBadgerDBEntityNotFound
	})
}

func (b *Storage) fetchAll(v interface{}, prefix []byte) error {
	t := reflect.TypeOf(v).Elem()
	if t.Kind() != reflect.Slice {
		return fmt.Errorf("%v: %T", errBadgerDBWrongEntityType, v)
	}
	s := reflect.ValueOf(v).Elem()
	return b.forEachKeyAndValue(prefix, func(k, val []byte) error {
		e := reflect.New(t.Elem()).Elem()
		i := e.Addr().Interface()
		gd, ok := i.(model.GobDeserializer)
		if !ok {
			return fmt.Errorf("%v: %T", errBadgerDBWrongEntityType, i)
		}
		gd.FromGob(gob.NewDecoder(bytes.NewReader(val)))
		s.Set(reflect.Append(s, e))
		return nil
	})
	return nil
}

func (b *Storage) getVal(key []byte, txn *badger.Txn) ([]byte, error) {
	item, err := txn.Get(key)
	switch err {
	case nil:
		break
	case badger.ErrKeyNotFound:
		return nil, nil
	default:
		return nil, err
	}
	return item.Value()
}

func (b *Storage) forEachKey(prefix []byte, f func(k []byte) error) error {
	return b.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		iter := txn.NewIterator(opts)
		defer iter.Close()

		for iter.Seek(prefix); iter.ValidForPrefix(prefix); iter.Next() {
			it := iter.Item()
			if err := f(it.Key()); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *Storage) forEachKeyAndValue(prefix []byte, f func(k, v []byte) error) error {
	return b.db.View(func(txn *badger.Txn) error {
		iter := txn.NewIterator(badger.DefaultIteratorOptions)
		defer iter.Close()

		for iter.Seek(prefix); iter.ValidForPrefix(prefix); iter.Next() {
			it := iter.Item()
			val, err := it.Value()
			if err != nil {
				return err
			}
			if err := f(it.Key(), val); err != nil {
				return err
			}
		}
		return nil
	})
}
