package consensus

import (
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/vitelabs/go-vite/consensus/consensus_db"

	"github.com/pkg/errors"
	"github.com/vitelabs/go-vite/common/types"
	"github.com/vitelabs/go-vite/consensus/core"
	"github.com/vitelabs/go-vite/ledger"
	"github.com/vitelabs/go-vite/log15"
)

// Ensure that all nodes get same result
type teller struct {
	info *core.GroupInfo
	//voteCache map[int32]*electionResult
	//voteCache *lru.Cache
	rw   *chainRw
	algo core.Algo

	//cacheDb *consensus_db.ConsensusDB

	voteCache Cache

	mLog log15.Logger
}

const seedDuration = time.Minute * 10

func newTeller(info *core.GroupInfo, rw *chainRw, log log15.Logger, cacheDb Cache) *teller {
	t := &teller{rw: rw}
	//t.info = &membersInfo{genesisTime: genesisTime, memberCnt: memberCnt, interval: interval, perCnt: perCnt, randCnt: 2, LowestLimit: big.NewInt(1000)}
	t.info = info
	t.algo = core.NewAlgo(t.info)
	t.mLog = log.New("gid", info.Gid.String())
	t.mLog.Info("new teller.", "membersInfo", info.String())

	t.voteCache = cacheDb
	return t
}

func (self *teller) voteResults(b *ledger.SnapshotBlock, seeds *core.SeedInfo, voteIndex uint64) ([]types.Address, error) {
	head := self.rw.GetLatestSnapshotBlock()

	if b.Height > head.Height {
		return nil, errors.New("rollback happened, block height[" + strconv.FormatUint(b.Height, 10) + "], head height[" + strconv.FormatUint(head.Height, 10) + "]")
	}

	headH := ledger.HashHeight{Height: b.Height, Hash: b.Hash}
	addressList, e := self.calVotes(headH, seeds, voteIndex)
	if e != nil {
		return nil, e
	}
	return addressList, nil
}

func (self *teller) electionIndex(index uint64) (*electionResult, error) {
	sTime := self.info.GenVoteTime(index)

	voteIndex := self.info.Time2Index(sTime) - 1

	block, e := self.rw.GetSnapshotBeforeTime(sTime)
	if e != nil {
		self.mLog.Error("geSnapshotBeferTime fail.", "err", e)
		return nil, e
	}
	// todo
	self.mLog.Info(fmt.Sprintf("election index:%d,%s, voteTime:%s", index, block.Hash, sTime))
	seeds, err := self.rw.GetSeedsBeforeHashH(block, seedDuration)
	if err != nil {
		return nil, err
	}
	seed := core.NewSeedInfo(seeds)
	voteResults, err := self.voteResults(block, seed, voteIndex)
	if err != nil {
		return nil, err
	}

	plans := self.genPlan(index, voteResults, block)
	return plans, nil
}
func (self *teller) genPlan(index uint64, members []types.Address, hashH *ledger.SnapshotBlock) *electionResult {
	result := electionResult{}
	result.STime = self.info.GenSTime(index)
	result.ETime = self.info.GenETime(index)
	result.Plans = self.info.GenPlanByAddress(index, members)
	result.Index = index
	result.Hash = hashH.Hash
	result.Height = hashH.Height
	return &result
}

func (self *teller) voteDetails(index uint64) ([]*VoteDetails, *ledger.HashHeight, error) {
	sTime := self.voteTime(index)

	block, e := self.rw.GetSnapshotBeforeTime(sTime)
	if e != nil {
		self.mLog.Error("geSnapshotBeferTime fail.", "err", e)
		return nil, nil, e
	}

	headH := ledger.HashHeight{Height: block.Height, Hash: block.Hash}
	details, err := self.rw.CalVoteDetails(self.info.Gid, self.info, headH)
	return details, &headH, err
}

func (self *teller) voteDetailsBeforeTime(t time.Time) ([]*VoteDetails, *ledger.HashHeight, error) {
	block, e := self.rw.GetSnapshotBeforeTime(t)
	if e != nil {
		self.mLog.Error("geSnapshotBeferTime fail.", "err", e)
		return nil, nil, e
	}

	headH := ledger.HashHeight{Height: block.Height, Hash: block.Hash}
	details, err := self.rw.CalVoteDetails(self.info.Gid, self.info, headH)
	return details, &headH, err
}

func (self *teller) electionTime(t time.Time) (*electionResult, error) {
	index := self.info.Time2Index(t)
	return self.electionIndex(index)
}
func (self *teller) time2Index(t time.Time) uint64 {
	index := self.info.Time2Index(t)
	return index
}
func (self *teller) index2Time(i uint64) (time.Time, time.Time) {
	sTime := self.info.GenSTime(i)
	eTime := self.info.GenETime(i)
	return sTime, eTime
}

func (self *teller) voteTime(i uint64) time.Time {
	sTime := self.info.GenVoteTime(i)
	return sTime
}

func (self *teller) findSeed(votes []*core.Vote) int64 {
	result := big.NewInt(0)
	for _, v := range votes {
		result.Add(result, v.Balance)
	}
	return result.Int64()
}

func (self *teller) calVotes(hashH ledger.HashHeight, seed *core.SeedInfo, voteIndex uint64) ([]types.Address, error) {
	// load from cache
	r, ok := self.voteCacheGet(hashH.Hash)
	if ok {
		fmt.Println(fmt.Sprintf("hit cache voteIndex:%d,%s", voteIndex, hashH.Hash))
		return r, nil
	}
	// record vote
	votes, err := self.rw.CalVotes(self.info, hashH)
	if err != nil {
		return nil, err
	}
	successRate, err := self.rw.GetSuccessRateByHour(voteIndex)
	if err != nil {
		return nil, err
	}

	self.mLog.Info(fmt.Sprintf("[%d][%d]success rate log: %+v", hashH.Height, voteIndex, successRate))
	context := core.NewVoteAlgoContext(votes, &hashH, successRate, seed)
	// filter size of members
	finalVotes := self.algo.FilterVotes(context)
	// shuffle the members
	finalVotes = self.algo.ShuffleVotes(finalVotes, &hashH, seed)

	address := core.ConvertVoteToAddress(finalVotes)

	// update cache
	self.voteCachePut(hashH.Hash, address)
	return address, nil
}
func (self *teller) voteCacheGet(hashes types.Hash) ([]types.Address, bool) {
	if self.voteCache == nil {
		return nil, false
	}
	value, ok := self.voteCache.Get(hashes)
	if ok {
		return value.(consensus_db.AddrArr), ok
	}
	return nil, ok
}

func (self *teller) voteCachePut(hashes types.Hash, addrArr []types.Address) {
	if self.voteCache != nil {
		self.voteCache.Add(hashes, addrArr)
	}
}
