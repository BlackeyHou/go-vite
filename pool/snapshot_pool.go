package pool

import (
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/vitelabs/go-vite/common"
	"github.com/vitelabs/go-vite/common/types"
	"github.com/vitelabs/go-vite/ledger"
	"github.com/vitelabs/go-vite/log15"
	"github.com/vitelabs/go-vite/verifier"
)

type snapshotPool struct {
	BCPool
	//rwMu *sync.RWMutex
	//consensus consensus.AccountsConsensus
	closed chan struct{}
	wg     sync.WaitGroup
	pool   *pool
	rw     *snapshotCh
	v      *snapshotVerifier
	f      *snapshotSyncer
}

func newSnapshotPoolBlock(block *ledger.SnapshotBlock, version *ForkVersion) *snapshotPoolBlock {
	return &snapshotPoolBlock{block: block, forkBlock: *newForkBlock(version)}
}

type snapshotPoolBlock struct {
	forkBlock
	block *ledger.SnapshotBlock
}

func (self *snapshotPoolBlock) Height() uint64 {
	return self.block.Height
}

func (self *snapshotPoolBlock) Hash() types.Hash {
	return self.block.Hash
}

func (self *snapshotPoolBlock) PrevHash() types.Hash {
	return self.block.PrevHash
}

func newSnapshotPool(
	name string,
	version *ForkVersion,
	v *snapshotVerifier,
	f *snapshotSyncer,
	rw *snapshotCh,
	log log15.Logger,
) *snapshotPool {
	pool := &snapshotPool{}
	pool.Id = name
	pool.version = version
	pool.rw = rw
	pool.v = v
	pool.f = f
	pool.log = log.New("snapshotPool", name)
	return pool
}

func (self *snapshotPool) init(
	tools *tools,
	pool *pool) {
	//self.consensus = accountsConsensus
	self.pool = pool
	self.BCPool.init(self.rw, tools)
}

func (self *snapshotPool) loopCheckFork() {
	self.wg.Add(1)
	defer self.wg.Done()
	for {
		select {
		case <-self.closed:
			return
		default:
			self.checkFork()
			// check fork every 2 sec.
			time.Sleep(2 * time.Second)
		}
	}
}

func (self *snapshotPool) checkFork() {
	longest := self.LongestChain()
	current := self.CurrentChain()
	if longest.ChainId() == current.ChainId() {
		return
	}
	self.snapshotFork(longest, current)

}

func (self *snapshotPool) snapshotFork(longest *forkedChain, current *forkedChain) error {
	self.log.Warn("[try]snapshot chain start fork.", "longest", longest.ChainId(), "current", current.ChainId())
	self.pool.Lock()
	defer self.pool.UnLock()
	self.log.Warn("[lock]snapshot chain start fork.", "longest", longest.ChainId(), "current", current.ChainId())

	k, forked, err := self.getForkPoint(longest, current)
	if err != nil {
		self.log.Error("get snapshot forkPoint err.", "err", err)
		return err
	}
	if k == nil {
		self.log.Error("keypoint is empty.", "forked", forked.Height())
		return errors.New("key point is nil.")
	}
	keyPoint := k.(*snapshotPoolBlock)
	self.log.Info("fork point", "height", keyPoint.Height(), "hash", keyPoint.Hash())

	snapshots, accounts, e := self.rw.delToHeight(keyPoint.block.Height)
	if e != nil {
		return e
	}

	if len(snapshots) > 0 {
		err = self.rollbackCurrent(snapshots)
		if err != nil {
			return err
		}
	}

	if len(accounts) > 0 {
		err = self.pool.ForkAccounts(accounts)
		if err != nil {
			return err
		}
	}

	err = self.CurrentModifyToChain(longest, &ledger.HashHeight{Hash: forked.Hash(), Height: forked.Height()})
	if err != nil {
		return err
	}
	self.version.Inc()
	return nil
}

func (self *snapshotPool) loop() {
	self.wg.Add(1)
	defer self.wg.Done()
	for {
		select {
		case <-self.closed:
			return
		default:
			self.loopGenSnippetChains()
			self.loopAppendChains()
			self.loopFetchForSnippets()
			self.loopCheckCurrentInsert()
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func (self *snapshotPool) loopCheckCurrentInsert() {
	if self.chainpool.current.size() == 0 {
		return
	}
	stat, block := self.snapshotTryInsert()

	if stat != nil {
		if stat.verifyResult() == verifier.FAIL {
			self.insertVerifyFail(block.(*snapshotPoolBlock), stat)
		} else if stat.verifyResult() == verifier.PENDING {
			self.insertVerifyPending(block.(*snapshotPoolBlock), stat)
		}
	}
}

func (self *snapshotPool) snapshotTryInsert() (*poolSnapshotVerifyStat, commonBlock) {
	self.pool.RLock()
	defer self.pool.RUnLock()
	self.rMu.Lock()
	defer self.rMu.Unlock()

	pool := self.chainpool
	current := pool.current
	minH := current.tailHeight + 1
	headH := current.headHeight
L:
	for i := minH; i <= headH; i++ {
		block := current.getBlock(i, false)

		if !block.checkForkVersion() {
			block.resetForkVersion()
		}
		stat := self.v.verifySnapshot(block.(*snapshotPoolBlock))
		if !block.checkForkVersion() {
			block.resetForkVersion()
			continue
		}
		result := stat.verifyResult()
		switch result {
		case verifier.PENDING:
			return stat, block
		case verifier.FAIL:
			self.log.Error("snapshot verify fail."+stat.errMsg(),
				"hash", block.Hash(), "height", block.Height())
			return stat, block
		case verifier.SUCCESS:
			if block.Height() == current.tailHeight+1 {
				err := pool.writeToChain(current, block)
				if err != nil {
					self.log.Error("insert snapshot chain fail.",
						"hash", block.Hash(), "height", block.Height(), "err", err)
					break L
				} else {
					self.blockpool.afterInsert(block)
				}
			} else {
				break L
			}
		default:
			self.log.Crit("Unexpected things happened. ",
				"result", result, "hash", block.Hash(), "height", block.Height())
			break L
		}
	}
	return nil, nil
}
func (self *snapshotPool) Start() {
	self.closed = make(chan struct{})
	common.Go(self.loop)
	common.Go(self.loopCheckFork)
	self.log.Info("snapshot_pool started.")
}
func (self *snapshotPool) Stop() {
	close(self.closed)
	self.wg.Wait()
	self.log.Info("snapshot_pool stopped.")
}

func (self *snapshotPool) insertVerifyFail(b *snapshotPoolBlock, stat *poolSnapshotVerifyStat) {
	block := b.block
	results := stat.results

	accounts := make(map[types.Address]*ledger.HashHeight)

	for k, account := range block.SnapshotContent {
		result := results[k]
		if result == verifier.FAIL {
			accounts[k] = account
		}
	}

	if len(accounts) > 0 {
		self.forkAccounts(b, accounts)
	}
}

func (self *snapshotPool) forkAccounts(b *snapshotPoolBlock, accounts map[types.Address]*ledger.HashHeight) {
	self.pool.Lock()
	defer self.pool.UnLock()

	for k, v := range accounts {
		self.pool.ForkAccountTo(k, v)
	}

	self.version.Inc()
}

func (self *snapshotPool) insertVerifyPending(b *snapshotPoolBlock, stat *poolSnapshotVerifyStat) {
	block := b.block

	results := stat.results

	accounts := make(map[types.Address]*ledger.HashHeight)

	for k, account := range block.SnapshotContent {
		result := results[k]
		if result == verifier.PENDING {
			hashH, e := self.pool.PendingAccountTo(k, account)
			if e != nil {
				self.log.Error("pending for account fail.", "err", e, "address", k, "hashH", account)
			}
			if hashH != nil {
				accounts[k] = account
			}
		}
	}
	if len(accounts) > 0 {
		self.forkAccounts(b, accounts)
	}
}

func (self *snapshotPool) AddDirectBlock(block *snapshotPoolBlock) error {
	self.rMu.Lock()
	defer self.rMu.Unlock()

	stat := self.v.verifySnapshot(block)
	result := stat.verifyResult()
	switch result {
	case verifier.PENDING:
		return errors.New("pending for something")
	case verifier.FAIL:
		return errors.New(stat.errMsg())
	case verifier.SUCCESS:
		err := self.chainpool.diskChain.rw.insertBlock(block)
		if err != nil {
			return err
		}
		head := self.chainpool.diskChain.Head()
		self.chainpool.insertNotify(head)
		return nil
	default:
		self.log.Crit("verify unexpected.")
		return errors.New("verify unexpected")
	}
}
