package chain_index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/syndtr/goleveldb/leveldb"
	errors2 "github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/vitelabs/go-vite/chain_db/database"
	"github.com/vitelabs/go-vite/common/types"
	"github.com/vitelabs/go-vite/config"
	"github.com/vitelabs/go-vite/ledger"
	"github.com/vitelabs/go-vite/log15"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	DBKP_BLOCK_LIST_BY_TOKEN = byte(1)

	DBKP_ACCOUNT_TOKEN_META = byte(2)

	DBKP_FILTER_TOKEN_CONSUME_ID = byte(3)

	DBKP_HEAD_HASH = byte(4)
)

const (
	STOP  = 1
	START = 2
)

type FilterTokenIndex struct {
	db *leveldb.DB

	dataDirName      string
	log              log15.Logger
	chainInstance    Chain
	EventNumPerBatch uint64

	status     int
	statusLock sync.Mutex
	ticker     *time.Ticker
	terminal   chan struct{}
	wg         sync.WaitGroup

	buildLock sync.Mutex
}

func NewFilterTokenIndex(cfg *config.Config, chainInstance Chain) (*FilterTokenIndex, error) {
	// register
	fti := &FilterTokenIndex{
		log:         log15.New("module", "filter_token"),
		dataDirName: filepath.Join(cfg.DataDir, "ledger_index"),

		chainInstance:    chainInstance,
		EventNumPerBatch: 1000,

		status: STOP,
	}

	err := fti.initDb()
	if err != nil {
		err := errors.New("initDb failed, error is " + err.Error())
		fti.log.Error(err.Error(), "method", "NewFilterTokenIndex")

		return nil, err
	}

	return fti, nil
}

func (fti *FilterTokenIndex) Start() {
	fti.statusLock.Lock()
	defer fti.statusLock.Unlock()
	if fti.status == START {
		return
	}

	if err := fti.checkAndInitData(); err != nil {
		fti.log.Crit("FilterTokenIndex start failed, error is "+err.Error(), "method", "Start")
	}
	if err := fti.build(); err != nil {
		fti.log.Error("fti build failed, error is "+err.Error(), "method", "Start")
	}
	fti.ticker = time.NewTicker(time.Second * 3)
	fti.wg.Add(1)
	go func() {
		defer fti.wg.Done()
		for {
			select {
			case <-fti.ticker.C:
				if err := fti.build(); err != nil {
					fti.log.Error("fti build failed, error is "+err.Error(), "method", "Start")
				}
			case <-fti.terminal:
				return
			}
		}
	}()

	fti.status = START
}

func (fti *FilterTokenIndex) Stop() {
	fti.statusLock.Lock()
	defer fti.statusLock.Unlock()

	if fti.status == STOP {
		return
	}

	fti.ticker.Stop()
	close(fti.terminal)
	fti.wg.Wait()
	fti.status = STOP

}

func (fti *FilterTokenIndex) initDb() error {
	db, err := database.NewLevelDb(fti.dataDirName)
	if err != nil {
		switch err.(type) {
		case *errors2.ErrCorrupted:
			// clear
			return fti.clearAndInitDb()
		default:
			fti.log.Error("NewLevelDb failed, error is "+err.Error(), "method", "initDb")
			return nil
		}
	}

	if db == nil {
		err := errors.New("NewFilterTokenIndex failed, db is nil")
		fti.log.Error(err.Error(), "method", "initDb")
		return err
	}

	fti.db = db

	return nil
}

func (fti *FilterTokenIndex) checkAndInitData() error {
	if isConsistency, err := fti.checkConsistency(); err != nil {
		fti.log.Error("checkConsistency failed, error is "+err.Error(), "method", "initDb")
		return err
	} else if !isConsistency {
		return fti.clearAndInitDb()
	}
	return nil
}

func (fti *FilterTokenIndex) checkConsistency() (bool, error) {
	latestBlockEventId, err := fti.chainInstance.GetLatestBlockEventId()
	if err != nil {
		return false, err
	}

	consumeId, err := fti.getConsumeId()
	if err != nil {
		if err == leveldb.ErrNotFound {
			return true, nil
		}
		return false, err
	}

	if consumeId > latestBlockEventId {
		return false, nil
	}
	return true, nil

}

func (fti *FilterTokenIndex) clearAndInitDb() error {
	if fti.db != nil {
		if closeErr := fti.db.Close(); closeErr != nil {
			return errors.New("Close db failed, error is " + closeErr.Error())
		}
	}

	if err := os.RemoveAll(fti.dataDirName); err != nil && err != os.ErrNotExist {
		return errors.New("Remove " + fti.dataDirName + " failed, error is " + err.Error())
	}

	fti.db = nil
	return fti.initDb()
}

func (fti *FilterTokenIndex) updateConsumeId(eventId uint64) error {
	key, _ := database.EncodeKey(DBKP_FILTER_TOKEN_CONSUME_ID)

	eventIdBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(eventIdBytes, eventId)

	return fti.db.Put(key, eventIdBytes, nil)
}

func (fti *FilterTokenIndex) getConsumeId() (uint64, error) {
	key, _ := database.EncodeKey(DBKP_FILTER_TOKEN_CONSUME_ID)
	value, err := fti.db.Get(key, nil)

	if err != nil {
		return 0, err
	}

	if len(value) <= 0 {
		return 0, nil
	}

	return binary.BigEndian.Uint64(value), nil
}

func (fti *FilterTokenIndex) build() error {
	fti.buildLock.Lock()
	defer fti.buildLock.Unlock()

	consumeId, err := fti.getConsumeId()
	if err != nil {
		if err == leveldb.ErrNotFound {
			consumeId = 1
		} else {
			return err
		}
	}

	latestBeId, err := fti.chainInstance.GetLatestBlockEventId()
	if err != nil {
		return err
	}

	unsavedBlocks := make(map[types.Address][]*ledger.AccountBlock)

	notFoundBlocks := make(map[types.Hash]struct{})

	eventNum := uint64(0)
	for eventId := consumeId; eventId <= latestBeId; eventId++ {
		eventType, blockHashList, err := fti.chainInstance.GetEvent(eventId)
		if err != nil {
			return err
		}
		switch eventType {
		// AddAccountBlocksEvent = byte(1)
		case byte(1):
			for _, blockHash := range blockHashList {
				block, err := fti.chainInstance.GetAccountBlockByHash(&blockHash)
				if err != nil {
					// .log.Error("GetAccountBlockByHash failed, error is "+err.Error(), "method", "send")
					return err
				}

				if block == nil {
					notFoundBlocks[blockHash] = struct{}{}
					continue
				}

				//fti.AddBlocks(block.Meta.AccountId, []*ledger.AccountBlock{block})
				unsavedBlocks[block.AccountAddress] = append(unsavedBlocks[block.AccountAddress], block)
			}
		case byte(2):
			for _, hash := range blockHashList {
				if _, ok := notFoundBlocks[hash]; ok {
					delete(notFoundBlocks, hash)
					continue
				}
				if err := fti.deleteHash(hash); err != nil {
					return err
				}
			}
		}

		eventNum++
		if eventId >= latestBeId || eventNum >= fti.EventNumPerBatch {
			for addr, blocks := range unsavedBlocks {
				account, err := fti.chainInstance.GetAccount(&addr)
				if err != nil {
					return err
				}

				if err := fti.addBlocks(account.AccountId, blocks); err != nil {
					return err
				}
			}
			unsavedBlocks = make(map[types.Address][]*ledger.AccountBlock)
			eventNum = 0

			if err := fti.updateConsumeId(eventId); err != nil {
				return err
			}
		}

	}

	return nil
}

func (fti *FilterTokenIndex) getHeadHash(accountId uint64, tokenTypeId types.TokenTypeId) (*types.Hash, error) {
	key, _ := database.EncodeKey(DBKP_ACCOUNT_TOKEN_META, accountId, tokenTypeId.Bytes())
	value, err := fti.db.Get(key, nil)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}

	hash, err := types.BytesToHash(value)
	return &hash, err
}

func (fti *FilterTokenIndex) saveHeadHash(batch *leveldb.Batch, accountId uint64, tokenTypeId types.TokenTypeId, hash types.Hash) {
	key, _ := database.EncodeKey(DBKP_ACCOUNT_TOKEN_META, accountId, tokenTypeId.Bytes())
	value := hash.Bytes()

	batch.Put(key, value)

	key2, _ := database.EncodeKey(DBKP_HEAD_HASH, hash.Bytes())
	accountIdBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(accountIdBytes, accountId)
	value2 := append(accountIdBytes, tokenTypeId.Bytes()...)

	batch.Put(key2, value2)
}

func (fti *FilterTokenIndex) deleteHash(headHash types.Hash) error {
	key, _ := database.EncodeKey(DBKP_HEAD_HASH, headHash.Bytes())
	value, err := fti.db.Get(key, nil)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return nil
		}
		return err
	}
	if len(value) <= 0 {
		return nil
	}

	accountId := binary.BigEndian.Uint64(value[:8])
	tokenTypeId, err := types.BytesToTokenTypeId(value[8:])
	if err != nil {
		return err
	}

	batch := new(leveldb.Batch)
	newHeadHash := &headHash

	for {

		prevHash, err := fti.getPrevHash(*newHeadHash)
		if err != nil {
			return err
		}
		newHeadHash = prevHash

		if newHeadHash == nil {
			break
		}

		isExisted, err := fti.chainInstance.IsAccountBlockExisted(*newHeadHash)
		if err != nil {
			return err
		}

		if isExisted {
			break
		}
	}
	fti.deleteHeadHash(batch, accountId, tokenTypeId)
	fti.deleteHeadHashIndex(batch, headHash)

	if newHeadHash != nil {
		fti.saveHeadHash(batch, accountId, tokenTypeId, *newHeadHash)
	}

	return fti.db.Write(batch, nil)
}

func (fti *FilterTokenIndex) getPrevHash(hash types.Hash) (*types.Hash, error) {
	key, _ := database.EncodeKey(DBKP_BLOCK_LIST_BY_TOKEN, hash.Bytes())

	value, err := fti.db.Get(key, nil)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}

	if len(value) > 0 {
		prevHash, err := types.BytesToHash(value)
		if err != nil {
			return nil, err
		}
		return &prevHash, nil
	}
	return nil, nil

}

func (fti *FilterTokenIndex) deleteHeadHashIndex(batch *leveldb.Batch, hash types.Hash) {
	key, _ := database.EncodeKey(DBKP_HEAD_HASH, hash.Bytes())

	batch.Delete(key)
}

func (fti *FilterTokenIndex) deleteHeadHash(batch *leveldb.Batch, accountId uint64, tokenTypeId types.TokenTypeId) {
	key, _ := database.EncodeKey(DBKP_ACCOUNT_TOKEN_META, accountId, tokenTypeId.Bytes())

	batch.Delete(key)
}

func (fti *FilterTokenIndex) isExisted(hash *types.Hash) (bool, error) {
	key, _ := database.EncodeKey(DBKP_BLOCK_LIST_BY_TOKEN, hash.Bytes())
	return fti.db.Has(key, nil)
}

func (fti *FilterTokenIndex) addBlocks(accountId uint64, blocks []*ledger.AccountBlock) error {
	// add
	batch := new(leveldb.Batch)

	unsavedHeadHash := make(map[types.TokenTypeId]types.Hash)

	for _, block := range blocks {
		if isExited, err := fti.isExisted(&block.Hash); err != nil {
			return err
		} else if isExited {
			continue
		}
		key, _ := database.EncodeKey(DBKP_BLOCK_LIST_BY_TOKEN, block.Hash.Bytes())

		tokenId, err := fti.getBlockTokenId(block)
		if err != nil {
			return err
		}

		var prevHashInToken *types.Hash
		if hash, ok := unsavedHeadHash[tokenId]; ok {
			prevHashInToken = &hash
		} else {
			var err error
			prevHashInToken, err = fti.getHeadHash(accountId, tokenId)

			if err != nil {
				return err
			}
		}

		var value []byte
		if prevHashInToken != nil {
			value = prevHashInToken.Bytes()
		}

		// batch write
		batch.Put(key, value)

		unsavedHeadHash[tokenId] = block.Hash
	}

	// save head hash
	for tokenTypeId, headHash := range unsavedHeadHash {
		fti.saveHeadHash(batch, accountId, tokenTypeId, headHash)
	}

	return fti.db.Write(batch, nil)
}

func (fti *FilterTokenIndex) getBlockTokenId(block *ledger.AccountBlock) (types.TokenTypeId, error) {
	if fti.chainInstance.IsGenesisAccountBlock(block) {
		return types.TokenTypeId{}, nil
	}

	if block.IsSendBlock() {
		return block.TokenId, nil
	}

	sendBlock, err := fti.chainInstance.GetAccountBlockByHash(&block.FromBlockHash)
	if err != nil {
		return types.TokenTypeId{}, err
	}
	if sendBlock == nil {

		err := errors.New("sendBlock is nil")
		return types.TokenTypeId{}, err
	}
	return sendBlock.TokenId, nil
}

func (fti *FilterTokenIndex) GetBlockHashList(account *ledger.Account, originBlockHash *types.Hash, tokenTypeId types.TokenTypeId, count uint64) ([]types.Hash, error) {
	var headHash *types.Hash
	if originBlockHash == nil {
		var err error
		headHash, err = fti.getHeadHash(account.AccountId, tokenTypeId)
		if err != nil {
			return nil, err
		}
		if headHash == nil {
			return nil, nil
		}
	} else {
		headHash = originBlockHash
		if isExisted, err := fti.isExisted(headHash); err != nil {
			return nil, err
		} else if !isExisted {
			err := errors.New(fmt.Sprintf("block %s is not exited", headHash))
			return nil, err
		}

	}

	hashList := []types.Hash{*headHash}

	currentHash := *headHash
	for i := uint64(1); i < count; i++ {
		prevHash, err := fti.getPrevHash(currentHash)

		if err != nil {
			return nil, err
		}

		if prevHash == nil {
			break
		}

		hashList = append(hashList, *prevHash)
		currentHash = *prevHash
	}
	return hashList, nil
}
