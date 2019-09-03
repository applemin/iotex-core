// Copyright (c) 2019 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package blockdao

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/golang/protobuf/proto"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/blockchain/block"
	"github.com/iotexproject/iotex-core/blockindex"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/db"
	"github.com/iotexproject/iotex-core/pkg/cache"
	"github.com/iotexproject/iotex-core/pkg/compress"
	"github.com/iotexproject/iotex-core/pkg/enc"
	"github.com/iotexproject/iotex-core/pkg/lifecycle"
	"github.com/iotexproject/iotex-core/pkg/prometheustimer"
	"github.com/iotexproject/iotex-core/pkg/util/byteutil"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
)

const (
	blockNS                  = "blk"
	blockHashHeightMappingNS = "h2h"
	blockHeaderNS            = "bhr"
	blockBodyNS              = "bbd"
	blockFooterNS            = "bfr"
	receiptsNS               = "rpt"
)

var (
	topHeightKey       = []byte("th")
	topHashKey         = []byte("ts")
	hashPrefix         = []byte("ha.")
	heightPrefix       = []byte("he.")
	heightToFilePrefix = []byte("hf.")
)

var (
	cacheMtc = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "iotex_blockdao_cache",
			Help: "IoTeX blockdao cache counter.",
		},
		[]string{"result"},
	)
	patternLen = len("00000000.db")
	suffixLen  = len(".db")
	// ErrNotOpened indicates db is not opened
	ErrNotOpened = errors.New("DB is not opened")
)

// BlockDAO represents the block data access object
type BlockDAO interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	GetBlockHash(uint64) (hash.Hash256, error)
	GetBlockHeight(hash.Hash256) (uint64, error)
	GetBlock(hash.Hash256) (*block.Block, error)
	GetBlockByHeight(uint64) (*block.Block, error)
	GetTipHeight() (uint64, error)
	GetTipHash() (hash.Hash256, error)
	Header(hash.Hash256) (*block.Header, error)
	Body(hash.Hash256) (*block.Body, error)
	Footer(hash.Hash256) (*block.Footer, error)
	GetReceiptByActionHash(hash.Hash256) (*action.Receipt, error)
	GetReceipts(uint64) ([]*action.Receipt, error)
	PutBlock(*block.Block) error
	PutReceipts(uint64, []*action.Receipt) error
	DeleteTipBlock() error
	KVStore() db.KVStore
}

type blockDAO struct {
	writeBlockIndex  bool
	writeActionIndex bool
	compressBlock    bool
	kvstore          db.KVStore
	indexer          blockindex.Indexer
	kvstores         sync.Map //store like map[index]db.KVStore,index from 1...N
	topIndex         atomic.Value
	timerFactory     *prometheustimer.TimerFactory
	lifecycle        lifecycle.Lifecycle
	headerCache      *cache.ThreadSafeLruCache
	bodyCache        *cache.ThreadSafeLruCache
	footerCache      *cache.ThreadSafeLruCache
	cfg              config.DB
	mutex            sync.Mutex // for create new db file
}

// NewBlockDAO instantiates a block DAO
func NewBlockDAO(kvstore db.KVStore, indexer blockindex.Indexer, gateway, async, compressBlock bool, cfg config.DB) BlockDAO {
	/*
	 * who should do indexing according to Chain.GatewayPlugin and Chain.EnableAsyncIndexWrite
	 *
	 * | Gateway | EnableAsyncIndexWrite | who to index | what to index
	 * |----------------------------------------------------------------
	 * |  False  |           x           | blockDAO     | block
	 * |  False  |           x           | blockDAO     | block
	 * |  True   |         False         | blockDAO     | block + action
	 * |  True   |         True          | IndexBuilder | block + action
	 */
	blockDAO := &blockDAO{
		writeBlockIndex:  !(gateway && async),
		writeActionIndex: gateway && !async,
		compressBlock:    compressBlock,
		kvstore:          kvstore,
		indexer:          indexer,
		cfg:              cfg,
	}
	if cfg.MaxCacheSize > 0 {
		blockDAO.headerCache = cache.NewThreadSafeLruCache(cfg.MaxCacheSize)
		blockDAO.bodyCache = cache.NewThreadSafeLruCache(cfg.MaxCacheSize)
		blockDAO.footerCache = cache.NewThreadSafeLruCache(cfg.MaxCacheSize)
	}
	timerFactory, err := prometheustimer.New(
		"iotex_block_dao_perf",
		"Performance of block DAO",
		[]string{"type"},
		[]string{"default"},
	)
	if err != nil {
		return nil
	}
	blockDAO.timerFactory = timerFactory
	blockDAO.lifecycle.Add(kvstore)
	blockDAO.lifecycle.Add(indexer)
	return blockDAO
}

// Start starts block DAO and initiates the top height if it doesn't exist
func (dao *blockDAO) Start(ctx context.Context) error {
	err := dao.lifecycle.OnStart(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to start child services")
	}
	// set init height value
	if _, err = dao.kvstore.Get(blockNS, topHeightKey); err != nil &&
		errors.Cause(err) == db.ErrNotExist {
		if err := dao.kvstore.Put(blockNS, topHeightKey, make([]byte, 8)); err != nil {
			return errors.Wrap(err, "failed to write initial value for top height")
		}
	}
	return dao.initStores()
}

func (dao *blockDAO) initStores() error {
	cfg := dao.cfg
	model, dir := getFileNameAndDir(cfg.DbPath)
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	var maxN uint64
	for _, file := range files {
		name := file.Name()
		lens := len(name)
		if lens < patternLen || !strings.Contains(name, model) {
			continue
		}
		num := name[lens-patternLen : lens-suffixLen]
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		dao.openDB(uint64(n))
		if uint64(n) > maxN {
			maxN = uint64(n)
		}
	}
	if maxN == 0 {
		maxN = 1
	}
	dao.topIndex.Store(maxN)
	return nil
}

func (dao *blockDAO) Stop(ctx context.Context) error { return dao.lifecycle.OnStop(ctx) }

func (dao *blockDAO) GetBlockHash(height uint64) (hash.Hash256, error) {
	return dao.getBlockHash(height)
}

func (dao *blockDAO) GetBlockHeight(hash hash.Hash256) (uint64, error) {
	return dao.getBlockHeight(hash)
}

func (dao *blockDAO) GetBlock(hash hash.Hash256) (*block.Block, error) {
	return dao.getBlock(hash)
}

func (dao *blockDAO) GetBlockByHeight(height uint64) (*block.Block, error) {
	hash, err := dao.getBlockHash(height)
	if err != nil {
		return nil, err
	}
	return dao.getBlock(hash)
}

func (dao *blockDAO) GetTipHash() (hash.Hash256, error) {
	return dao.getTipHash()
}

func (dao *blockDAO) GetTipHeight() (uint64, error) {
	return dao.getTipHeight()
}

func (dao *blockDAO) Header(h hash.Hash256) (*block.Header, error) {
	return dao.header(h)
}

func (dao *blockDAO) Body(h hash.Hash256) (*block.Body, error) {
	return dao.body(h)
}

func (dao *blockDAO) Footer(h hash.Hash256) (*block.Footer, error) {
	return dao.footer(h)
}

func (dao *blockDAO) GetReceiptByActionHash(h hash.Hash256) (*action.Receipt, error) {
	return dao.getReceiptByActionHash(h)
}

func (dao *blockDAO) GetReceipts(blkHeight uint64) ([]*action.Receipt, error) {
	return dao.getReceipts(blkHeight)
}

func (dao *blockDAO) PutBlock(blk *block.Block) error {
	return dao.putBlock(blk)
}

func (dao *blockDAO) PutReceipts(blkHeight uint64, blkReceipts []*action.Receipt) error {
	return dao.putReceipts(blkHeight, blkReceipts)
}

func (dao *blockDAO) DeleteTipBlock() error {
	return dao.deleteTipBlock()
}

func (dao *blockDAO) KVStore() db.KVStore {
	return dao.kvstore
}

// getBlockHash returns the block hash by height
func (dao *blockDAO) getBlockHash(height uint64) (hash.Hash256, error) {
	if height == 0 {
		return hash.ZeroHash256, nil
	}
	key := append(heightPrefix, byteutil.Uint64ToBytes(height)...)
	value, err := dao.kvstore.Get(blockHashHeightMappingNS, key)
	hash := hash.ZeroHash256
	if err != nil {
		return hash, errors.Wrap(err, "failed to get block hash")
	}
	if len(hash) != len(value) {
		return hash, errors.Wrap(err, "blockhash is broken")
	}
	copy(hash[:], value)
	return hash, nil
}

// getBlockHeight returns the block height by hash
func (dao *blockDAO) getBlockHeight(hash hash.Hash256) (uint64, error) {
	key := append(hashPrefix, hash[:]...)
	value, err := dao.kvstore.Get(blockHashHeightMappingNS, key)
	if err != nil {
		return 0, errors.Wrap(err, "failed to get block height")
	}
	if len(value) == 0 {
		return 0, errors.Wrapf(db.ErrNotExist, "height missing for block with hash = %x", hash)
	}
	return enc.MachineEndian.Uint64(value), nil
}

// getBlock returns a block
func (dao *blockDAO) getBlock(hash hash.Hash256) (*block.Block, error) {
	header, err := dao.header(hash)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get block header %x", hash)
	}
	body, err := dao.body(hash)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get block body %x", hash)
	}
	footer, err := dao.footer(hash)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get block footer %x", hash)
	}
	return &block.Block{
		Header: *header,
		Body:   *body,
		Footer: *footer,
	}, nil
}

func (dao *blockDAO) header(h hash.Hash256) (*block.Header, error) {
	if dao.headerCache != nil {
		header, ok := dao.headerCache.Get(h)
		if ok {
			cacheMtc.WithLabelValues("hit_header").Inc()
			return header.(*block.Header), nil
		}
		cacheMtc.WithLabelValues("miss_header").Inc()
	}
	value, err := dao.getBlockValue(blockHeaderNS, h)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get block header %x", h)
	}
	if dao.compressBlock {
		timer := dao.timerFactory.NewTimer("decompress_header")
		value, err = compress.Decompress(value)
		timer.End()
		if err != nil {
			return nil, errors.Wrapf(err, "error when decompressing a block header %x", h)
		}
	}
	if len(value) == 0 {
		return nil, errors.Wrapf(db.ErrNotExist, "block header %x is missing", h)
	}
	header := &block.Header{}
	if err := header.Deserialize(value); err != nil {
		return nil, errors.Wrapf(err, "failed to deserialize block header %x", h)
	}
	if dao.headerCache != nil {
		dao.headerCache.Add(h, header)
	}
	return header, nil
}

func (dao *blockDAO) body(h hash.Hash256) (*block.Body, error) {
	if dao.bodyCache != nil {
		body, ok := dao.bodyCache.Get(h)
		if ok {
			cacheMtc.WithLabelValues("hit_body").Inc()
			return body.(*block.Body), nil
		}
		cacheMtc.WithLabelValues("miss_body").Inc()
	}
	value, err := dao.getBlockValue(blockBodyNS, h)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get block body %x", h)
	}
	if dao.compressBlock {
		timer := dao.timerFactory.NewTimer("decompress_body")
		value, err = compress.Decompress(value)
		timer.End()
		if err != nil {
			return nil, errors.Wrapf(err, "error when decompressing a block body %x", h)
		}
	}
	if len(value) == 0 {
		return nil, errors.Wrapf(db.ErrNotExist, "block body %x is missing", h)
	}
	body := &block.Body{}
	if err := body.Deserialize(value); err != nil {
		return nil, errors.Wrapf(err, "failed to deserialize block body %x", h)
	}
	if dao.bodyCache != nil {
		dao.bodyCache.Add(h, body)
	}
	return body, nil
}

func (dao *blockDAO) footer(h hash.Hash256) (*block.Footer, error) {
	if dao.footerCache != nil {
		footer, ok := dao.footerCache.Get(h)
		if ok {
			cacheMtc.WithLabelValues("hit_footer").Inc()
			return footer.(*block.Footer), nil
		}
		cacheMtc.WithLabelValues("miss_footer").Inc()
	}
	value, err := dao.getBlockValue(blockFooterNS, h)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get block footer %x", h)
	}
	if dao.compressBlock {
		timer := dao.timerFactory.NewTimer("decompress_footer")
		value, err = compress.Decompress(value)
		timer.End()
		if err != nil {
			return nil, errors.Wrapf(err, "error when decompressing a block footer %x", h)
		}
	}
	if len(value) == 0 {
		return nil, errors.Wrapf(db.ErrNotExist, "block footer %x is missing", h)
	}
	footer := &block.Footer{}
	if err := footer.Deserialize(value); err != nil {
		return nil, errors.Wrapf(err, "failed to deserialize block footer %x", h)
	}
	if dao.footerCache != nil {
		dao.footerCache.Add(h, footer)
	}
	return footer, nil
}

// getTipHeight returns the blockchain height
func (dao *blockDAO) getTipHeight() (uint64, error) {
	value, err := dao.kvstore.Get(blockNS, topHeightKey)
	if err != nil {
		return 0, errors.Wrap(err, "failed to get top height")
	}
	if len(value) == 0 {
		return 0, errors.Wrap(db.ErrNotExist, "blockchain height missing")
	}
	return enc.MachineEndian.Uint64(value), nil
}

// getTipHash returns the blockchain tip hash
func (dao *blockDAO) getTipHash() (hash.Hash256, error) {
	value, err := dao.kvstore.Get(blockNS, topHashKey)
	if err != nil {
		return hash.ZeroHash256, errors.Wrap(err, "failed to get tip hash")
	}
	return hash.BytesToHash256(value), nil
}

// getReceiptByActionHash returns the receipt by execution hash
func (dao *blockDAO) getReceiptByActionHash(h hash.Hash256) (*action.Receipt, error) {
	height, err := dao.indexer.GetBlockHeightByActionHash(h)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get receipt index for action %x", h)
	}
	kvstore, _, err := dao.getDBFromHeight(height)
	if err != nil {
		return nil, err
	}
	receiptsBytes, err := kvstore.Get(receiptsNS, byteutil.Uint64ToBytes(height))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get receipts of block %d", height)
	}
	receipts := iotextypes.Receipts{}
	if err := proto.Unmarshal(receiptsBytes, &receipts); err != nil {
		return nil, err
	}
	for _, receipt := range receipts.Receipts {
		r := action.Receipt{}
		r.ConvertFromReceiptPb(receipt)
		if r.ActionHash == h {
			return &r, nil
		}
	}
	return nil, errors.Errorf("receipt of action %x isn't found", h)
}

func (dao *blockDAO) getReceipts(blkHeight uint64) ([]*action.Receipt, error) {
	kvstore, _, err := dao.getDBFromHeight(blkHeight)
	if err != nil {
		return nil, err
	}
	value, err := kvstore.Get(receiptsNS, byteutil.Uint64ToBytes(blkHeight))
	if err != nil {
		return nil, errors.Wrap(err, "failed to get receipts")
	}
	if len(value) == 0 {
		return nil, errors.Wrap(db.ErrNotExist, "block receipts missing")
	}
	receiptsPb := &iotextypes.Receipts{}
	if err := proto.Unmarshal(value, receiptsPb); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal block receipts")
	}
	var blockReceipts []*action.Receipt
	for _, receiptPb := range receiptsPb.Receipts {
		receipt := &action.Receipt{}
		receipt.ConvertFromReceiptPb(receiptPb)
		blockReceipts = append(blockReceipts, receipt)
	}
	return blockReceipts, nil
}

// putBlock puts a block
func (dao *blockDAO) putBlock(blk *block.Block) error {
	heightValue, err := dao.kvstore.Get(blockNS, topHeightKey)
	if err != nil {
		return errors.Wrap(err, "failed to get top height")
	}
	if blk.Height() <= enc.MachineEndian.Uint64(heightValue) {
		return errors.Errorf("block %d already exist", blk.Height())
	}

	serHeader, err := blk.Header.Serialize()
	if err != nil {
		return errors.Wrap(err, "failed to serialize block header")
	}
	serBody, err := blk.Body.Serialize()
	if err != nil {
		return errors.Wrap(err, "failed to serialize block body")
	}
	serFooter, err := blk.Footer.Serialize()
	if err != nil {
		return errors.Wrap(err, "failed to serialize block footer")
	}
	if dao.compressBlock {
		timer := dao.timerFactory.NewTimer("compress_header")
		serHeader, err = compress.Compress(serHeader)
		timer.End()
		if err != nil {
			return errors.Wrapf(err, "error when compressing a block header")
		}
		timer = dao.timerFactory.NewTimer("compress_body")
		serBody, err = compress.Compress(serBody)
		timer.End()
		if err != nil {
			return errors.Wrapf(err, "error when compressing a block body")
		}
		timer = dao.timerFactory.NewTimer("compress_footer")
		serFooter, err = compress.Compress(serFooter)
		timer.End()
		if err != nil {
			return errors.Wrapf(err, "error when compressing a block footer")
		}
	}
	batchForBlock := db.NewBatch()
	hash := blk.HashBlock()
	batchForBlock.Put(blockHeaderNS, hash[:], serHeader, "failed to put block header")
	batchForBlock.Put(blockBodyNS, hash[:], serBody, "failed to put block body")
	batchForBlock.Put(blockFooterNS, hash[:], serFooter, "failed to put block footer")
	kv, _, err := dao.getTopDB(blk.Height())
	if err != nil {
		return err
	}
	if err = kv.Commit(batchForBlock); err != nil {
		return err
	}

	batch := db.NewBatch()
	hashKey := append(hashPrefix, hash[:]...)
	heightValue = byteutil.Uint64ToBytes(blk.Height())
	batch.Put(blockHashHeightMappingNS, hashKey, heightValue, "failed to put hash -> height mapping")
	heightKey := append(heightPrefix, heightValue...)
	batch.Put(blockHashHeightMappingNS, heightKey, hash[:], "failed to put height -> hash mapping")
	batch.Put(blockNS, topHeightKey, heightValue, "failed to put top height")
	batch.Put(blockNS, topHashKey, hash[:], "failed to put top hash")
	if err = dao.kvstore.Commit(batch); err != nil {
		return err
	}

	// TODO: handle fileindex
	//heightToFile := append(heightToFilePrefix, height...)
	//fileindexBytes := byteutil.Uint64ToBytes(fileindex)
	//batch.Put(blockNS, heightToFile, fileindexBytes, "failed to put height -> file index mapping")

	if !dao.writeBlockIndex {
		return nil
	}
	if err = dao.indexer.IndexBlock(blk, dao.writeActionIndex); err != nil {
		return err
	}
	if !dao.writeActionIndex {
		return nil
	}
	if err := dao.indexer.IndexAction(blk); err != nil {
		return err
	}
	return dao.indexer.Commit()
}

// putReceipts store receipt into db
func (dao *blockDAO) putReceipts(blkHeight uint64, blkReceipts []*action.Receipt) error {
	kvstore, err := dao.getTopDBOfOpened(blkHeight)
	if err != nil {
		return err
	}
	if blkReceipts == nil {
		return nil
	}
	receipts := iotextypes.Receipts{}
	for _, r := range blkReceipts {
		receipts.Receipts = append(receipts.Receipts, r.ConvertToReceiptPb())
	}
	receiptsBytes, err := proto.Marshal(&receipts)
	if err != nil {
		return err
	}
	return kvstore.Put(receiptsNS, byteutil.Uint64ToBytes(blkHeight), receiptsBytes)
}

// deleteTipBlock deletes the tip block
func (dao *blockDAO) deleteTipBlock() error {
	// First obtain tip height from db
	height, err := dao.getTipHeight()
	if err != nil {
		return errors.Wrap(err, "failed to get tip height")
	}
	if height == 0 {
		// should not delete genesis block
		return errors.New("cannot delete genesis block")
	}
	// Obtain tip block hash
	hash, err := dao.getTipHash()
	if err != nil {
		return errors.Wrap(err, "failed to get tip block hash")
	}

	// Obtain block
	blk, err := dao.getBlock(hash)
	if err != nil {
		return errors.Wrap(err, "failed to get tip block")
	}

	batchForBlock := db.NewBatch()
	// Delete hash -> block mapping
	batchForBlock.Delete(blockHeaderNS, hash[:], "failed to delete block header")
	if dao.headerCache != nil {
		dao.headerCache.Remove(hash)
	}
	batchForBlock.Delete(blockBodyNS, hash[:], "failed to delete block body")
	if dao.bodyCache != nil {
		dao.bodyCache.Remove(hash)
	}
	batchForBlock.Delete(blockFooterNS, hash[:], "failed to delete block footer")
	if dao.footerCache != nil {
		dao.footerCache.Remove(hash)
	}
	// delete receipt
	batchForBlock.Delete(receiptsNS, byteutil.Uint64ToBytes(height), "failed to delete receipt")

	whichDB, _, err := dao.getDBFromHeight(height)
	if err != nil {
		return err
	}
	if err := whichDB.Commit(batchForBlock); err != nil {
		return err
	}

	// Update tip height
	if err := dao.kvstore.Put(blockNS, topHeightKey, byteutil.Uint64ToBytes(height-1)); err != nil {
		return errors.Wrap(err, "failed to update top height")
	}
	// Update tip hash
	hash, err = dao.getBlockHash(height - 1)
	if err != nil {
		return errors.Wrap(err, "failed to get tip block hash")
	}
	if err := dao.kvstore.Put(blockNS, topHashKey, hash[:]); err != nil {
		return errors.Wrap(err, "failed to update top hash")
	}

	if !dao.writeBlockIndex {
		return nil
	}
	// delete block index
	if err = dao.indexer.DeleteBlockIndex(blk); err != nil {
		return err
	}
	if !dao.writeActionIndex {
		return nil
	}
	// delete action index
	return dao.indexer.DeleteActionIndex(blk)
}

// getDBFromHash returns db of this block stored
func (dao *blockDAO) getDBFromHash(h hash.Hash256) (db.KVStore, uint64, error) {
	height, err := dao.getBlockHeight(h)
	if err != nil {
		return nil, 0, err
	}
	return dao.getDBFromHeight(height)
}

func (dao *blockDAO) getTopDB(blkHeight uint64) (kvstore db.KVStore, index uint64, err error) {
	if dao.cfg.SplitDBSizeMB == 0 {
		return dao.kvstore, 0, nil
	}
	if blkHeight <= dao.cfg.SplitDBHeight {
		return dao.kvstore, 0, nil
	}
	topIndex := dao.topIndex.Load().(uint64)
	file, dir := getFileNameAndDir(dao.cfg.DbPath)
	if err != nil {
		return
	}
	longFileName := dir + "/" + file + fmt.Sprintf("-%08d", topIndex) + ".db"
	dat, err := os.Stat(longFileName)
	if err != nil && os.IsNotExist(err) {
		// db file is not exist,this will create
		return dao.openDB(topIndex)
	}
	// other errors except file is not exist
	if err != nil {
		return
	}
	// file exists,but need create new db
	if uint64(dat.Size()) > dao.cfg.SplitDBSize() {
		kvstore, index, err = dao.openDB(topIndex + 1)
		dao.topIndex.Store(index)
		return
	}
	// db exist,need load from kvstores
	kv, ok := dao.kvstores.Load(topIndex)
	if ok {
		kvstore, ok = kv.(db.KVStore)
		if !ok {
			err = errors.New("db convert error")
		}
		index = topIndex
		return
	}
	// file exists,but not opened
	return dao.openDB(topIndex)
}

func (dao *blockDAO) getTopDBOfOpened(blkHeight uint64) (kvstore db.KVStore, err error) {
	if dao.cfg.SplitDBSizeMB == 0 {
		return dao.kvstore, nil
	}
	if blkHeight <= dao.cfg.SplitDBHeight {
		return dao.kvstore, nil
	}
	topIndex := dao.topIndex.Load().(uint64)
	kv, ok := dao.kvstores.Load(topIndex)
	if ok {
		kvstore, ok = kv.(db.KVStore)
		if !ok {
			err = errors.New("db convert error")
		}
		return
	}
	err = ErrNotOpened
	return
}

func (dao *blockDAO) getDBFromHeight(blkHeight uint64) (kvstore db.KVStore, index uint64, err error) {
	if dao.cfg.SplitDBSizeMB == 0 {
		return dao.kvstore, 0, nil
	}
	if blkHeight <= dao.cfg.SplitDBHeight {
		return dao.kvstore, 0, nil
	}
	hei := byteutil.Uint64ToBytes(blkHeight)
	heightToFile := append(heightToFilePrefix, hei...)
	value, err := dao.kvstore.Get(blockNS, heightToFile[:])
	if err != nil {
		return
	}
	heiIndex := enc.MachineEndian.Uint64(value)
	return dao.getDBFromIndex(heiIndex)
}

func (dao *blockDAO) getDBFromIndex(idx uint64) (kvstore db.KVStore, index uint64, err error) {
	if idx == 0 {
		return dao.kvstore, 0, nil
	}
	kv, ok := dao.kvstores.Load(idx)
	if ok {
		kvstore, ok = kv.(db.KVStore)
		if !ok {
			err = errors.New("db convert error")
		}
		index = idx
		return
	}
	// if user rm some db files manully,then call this method will create new file
	return dao.openDB(idx)
}

// getBlockValue get block's data from db,if this db failed,it will try the previous one
func (dao *blockDAO) getBlockValue(blockNS string, h hash.Hash256) ([]byte, error) {
	whichDB, index, err := dao.getDBFromHash(h)
	if err != nil {
		return nil, err
	}
	value, err := whichDB.Get(blockNS, h[:])
	if errors.Cause(err) == db.ErrNotExist {
		idx := index - 1
		if idx < 0 {
			idx = 0
		}
		db, _, err := dao.getDBFromIndex(idx)
		if err != nil {
			return nil, err
		}
		value, err = db.Get(blockNS, h[:])
	}
	return value, err
}

// openDB open file if exists, or create new file
func (dao *blockDAO) openDB(idx uint64) (kvstore db.KVStore, index uint64, err error) {
	if idx == 0 {
		return dao.kvstore, 0, nil
	}
	dao.mutex.Lock()
	defer dao.mutex.Unlock()
	cfg := dao.cfg
	model, _ := getFileNameAndDir(cfg.DbPath)
	name := model + fmt.Sprintf("-%08d", idx) + ".db"

	// open or create this db file
	cfg.DbPath = path.Dir(cfg.DbPath) + "/" + name
	kvstore = db.NewBoltDB(cfg)
	dao.kvstores.Store(idx, kvstore)
	err = kvstore.Start(context.Background())
	if err != nil {
		return
	}
	dao.lifecycle.Add(kvstore)
	index = idx
	return
}

func getFileNameAndDir(p string) (fileName, dir string) {
	var withSuffix, suffix string
	withSuffix = path.Base(p)
	suffix = path.Ext(withSuffix)
	fileName = strings.TrimSuffix(withSuffix, suffix)
	dir = path.Dir(p)
	return
}
