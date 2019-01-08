// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package state provides a caching layer atop the Ethereum state trie.
// 1.存储所有的账户信息（stateObject）。
// 2.提供增删、修改账户的状态数据（stateObject）的接口。
// 3.Finalise和提交修改的账户信息（stateObject）。
// 4.对每个状态数据改变记录日志，创建快照，实现回滚。
package state

import (
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

type revision struct {
	id           int
	journalIndex int // 是日志（journal）的索引
}

var (
	// emptyState is the known hash of an empty state trie entry.
	emptyState = crypto.Keccak256Hash(nil)

	// emptyCode is the known hash of the empty EVM bytecode.
	emptyCode = crypto.Keccak256Hash(nil)
)

type proofList [][]byte

func (n *proofList) Put(key []byte, value []byte) error {
	*n = append(*n, value)
	return nil
}

// StateDBs within the ethereum protocol are used to store anything
// within the merkle trie. StateDBs take care of caching and storing
// nested states. It's the general query interface to retrieve:
// * Contracts
// * Accounts
// 在merkle树种保存任何数据，形式是kv
// stateObject存储在stateDB中，stateDB(一级缓存)存储在trie上，tire作为二级缓存，tire存在在db中(三级缓存)，保存在链上。
type StateDB struct {
	// 存储本Trie的数据库
	db Database
	// 存储所有的stateObject
	trie Trie

	// This map holds 'live' objects, which will get modified while processing a state transition.
	// 最近使用过的数据对象，他们的账户地址为key
	stateObjects map[common.Address]*stateObject // 包含的stateObject（是否可以理解成零级缓存?!）
	// 修改过的账户对象
	stateObjectsDirty map[common.Address]struct{} // "脏账户"，就是在StateDB中，账户已经被修改了，但是状态树中的值还没有修改，这些账户就被存到stateObjectsDirty中

	// DB error.
	// State objects are used by the consensus core and VM which are
	// unable to deal with database-level errors. Any error that occurs
	// during a database read is memoized here and will eventually be returned
	// by StateDB.Commit.
	dbErr error

	// The refund counter, also used by state transitioning.
	refund uint64

	thash, bhash common.Hash
	txIndex      int
	logs         map[common.Hash][]*types.Log
	logSize      uint

	preimages map[common.Hash][]byte

	// Journal of state modifications. This is the backbone of
	// Snapshot and RevertToSnapshot.
	// 快照和回滚的主要参数
	journal        *journal   // 日志 存的是账户变动的反向操作 这对以太坊实现快照功能以及回滚世界状态非常有用。
	validRevisions []revision // validRevisions是一个revision的切片，后者存的是日志（journal）的索引
	// 如何回滚快照
	// 通过revision的journalIndex可以索引到日志切片的位置，回滚到某个revision就只要去查找当前journals切片中大于revision.journalIndex的那些日志，并执行，即可回滚到当前revision指定的世界状态。
	nextRevisionId int
}

// 使用给定的树根创建树;实例化StateDB结构
// TOVIEW  调用时机:
// 1.BlockChain插入区块链时进行状态验证：BlockChain.inertChain —> state.New
// 2.BlockChain初始化时验证状态是否可读：BlockChain.loadLastState —> state.New
// Create a new state from a given trie.
func New(root common.Hash, db Database) (*StateDB, error) {
	tr, err := db.OpenTrie(root)
	if err != nil {
		return nil, err
	}
	return &StateDB{
		db:                db,
		trie:              tr,
		stateObjects:      make(map[common.Address]*stateObject),
		stateObjectsDirty: make(map[common.Address]struct{}),
		logs:              make(map[common.Hash][]*types.Log),
		preimages:         make(map[common.Hash][]byte),
		journal:           newJournal(),
	}, nil
}

// setError remembers the first non-nil error it is called with.
func (self *StateDB) setError(err error) {
	if self.dbErr == nil {
		self.dbErr = err
	}
}

func (self *StateDB) Error() error {
	return self.dbErr
}

// Reset clears out all ephemeral state objects from the state db, but keeps
// the underlying state trie to avoid reloading data for the next operations.
func (self *StateDB) Reset(root common.Hash) error {
	tr, err := self.db.OpenTrie(root)
	if err != nil {
		return err
	}
	self.trie = tr
	self.stateObjects = make(map[common.Address]*stateObject)
	self.stateObjectsDirty = make(map[common.Address]struct{})
	self.thash = common.Hash{}
	self.bhash = common.Hash{}
	self.txIndex = 0
	self.logs = make(map[common.Hash][]*types.Log)
	self.logSize = 0
	self.preimages = make(map[common.Hash][]byte)
	self.clearJournalAndRefund()
	return nil
}

func (self *StateDB) AddLog(log *types.Log) {
	self.journal.append(addLogChange{txhash: self.thash})

	log.TxHash = self.thash
	log.BlockHash = self.bhash
	log.TxIndex = uint(self.txIndex)
	log.Index = self.logSize
	self.logs[self.thash] = append(self.logs[self.thash], log)
	self.logSize++
}

func (self *StateDB) GetLogs(hash common.Hash) []*types.Log {
	return self.logs[hash]
}

func (self *StateDB) Logs() []*types.Log {
	var logs []*types.Log
	for _, lgs := range self.logs {
		logs = append(logs, lgs...)
	}
	return logs
}

// AddPreimage records a SHA3 preimage seen by the VM.
func (self *StateDB) AddPreimage(hash common.Hash, preimage []byte) {
	if _, ok := self.preimages[hash]; !ok {
		self.journal.append(addPreimageChange{hash: hash})
		pi := make([]byte, len(preimage))
		copy(pi, preimage)
		self.preimages[hash] = pi
	}
}

// Preimages returns a list of SHA3 preimages that have been submitted.
func (self *StateDB) Preimages() map[common.Hash][]byte {
	return self.preimages
}

// AddRefund adds gas to the refund counter
func (self *StateDB) AddRefund(gas uint64) {
	self.journal.append(refundChange{prev: self.refund})
	self.refund += gas
}

// SubRefund removes gas from the refund counter.
// This method will panic if the refund counter goes below zero
func (self *StateDB) SubRefund(gas uint64) {
	self.journal.append(refundChange{prev: self.refund})
	if gas > self.refund {
		panic("Refund counter below zero")
	}
	self.refund -= gas
}

// Exist reports whether the given account address exists in the state.
// Notably this also returns true for suicided accounts.
func (self *StateDB) Exist(addr common.Address) bool {
	return self.getStateObject(addr) != nil
}

// Empty returns whether the state object is either non-existent
// or empty according to the EIP161 specification (balance = nonce = code = 0)
func (self *StateDB) Empty(addr common.Address) bool {
	so := self.getStateObject(addr)
	return so == nil || so.empty()
}

// Retrieve the balance from the given address or 0 if object not found
func (self *StateDB) GetBalance(addr common.Address) *big.Int {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Balance()
	}
	return common.Big0
}

func (self *StateDB) GetNonce(addr common.Address) uint64 {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Nonce()
	}

	return 0
}

func (self *StateDB) GetCode(addr common.Address) []byte {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Code(self.db)
	}
	return nil
}

func (self *StateDB) GetCodeSize(addr common.Address) int {
	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return 0
	}
	if stateObject.code != nil {
		return len(stateObject.code)
	}
	size, err := self.db.ContractCodeSize(stateObject.addrHash, common.BytesToHash(stateObject.CodeHash()))
	if err != nil {
		self.setError(err)
	}
	return size
}

func (self *StateDB) GetCodeHash(addr common.Address) common.Hash {
	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return common.Hash{}
	}
	return common.BytesToHash(stateObject.CodeHash())
}

// GetState retrieves a value from the given account's storage trie.
func (self *StateDB) GetState(addr common.Address, hash common.Hash) common.Hash {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetState(self.db, hash)
	}
	return common.Hash{}
}

// GetProof returns the MerkleProof for a given Account
func (self *StateDB) GetProof(a common.Address) ([][]byte, error) {
	var proof proofList
	err := self.trie.Prove(crypto.Keccak256(a.Bytes()), 0, &proof)
	return [][]byte(proof), err
}

// GetProof returns the StorageProof for given key
func (self *StateDB) GetStorageProof(a common.Address, key common.Hash) ([][]byte, error) {
	var proof proofList
	trie := self.StorageTrie(a)
	if trie == nil {
		return proof, errors.New("storage trie for requested address does not exist")
	}
	err := trie.Prove(crypto.Keccak256(key.Bytes()), 0, &proof)
	return [][]byte(proof), err
}

// GetCommittedState retrieves a value from the given account's committed storage trie.
func (self *StateDB) GetCommittedState(addr common.Address, hash common.Hash) common.Hash {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetCommittedState(self.db, hash)
	}
	return common.Hash{}
}

// Database retrieves the low level database supporting the lower level trie ops.
func (self *StateDB) Database() Database {
	return self.db
}

// StorageTrie returns the storage trie of an account.
// The return value is a copy and is nil for non-existent accounts.
func (self *StateDB) StorageTrie(addr common.Address) Trie {
	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return nil
	}
	cpy := stateObject.deepCopy(self)
	return cpy.updateTrie(self.db)
}

func (self *StateDB) HasSuicided(addr common.Address) bool {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.suicided
	}
	return false
}

/*
 * SETTERS
 */

// AddBalance adds amount to the account associated with addr.
func (self *StateDB) AddBalance(addr common.Address, amount *big.Int) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.AddBalance(amount)
	}
}

// SubBalance subtracts amount from the account associated with addr.
func (self *StateDB) SubBalance(addr common.Address, amount *big.Int) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SubBalance(amount)
	}
}

func (self *StateDB) SetBalance(addr common.Address, amount *big.Int) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetBalance(amount)
	}
}

func (self *StateDB) SetNonce(addr common.Address, nonce uint64) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetNonce(nonce)
	}
}

func (self *StateDB) SetCode(addr common.Address, code []byte) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetCode(crypto.Keccak256Hash(code), code)
	}
}

func (self *StateDB) SetState(addr common.Address, key, value common.Hash) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetState(self.db, key, value)
	}
}

// Suicide marks the given account as suicided.
// This clears the account balance.
//
// The account's state object is still available until the state is committed,
// getStateObject will return a non-nil account after Suicide.
// 销毁账户
// 它的反向操作就是获取该stateObject，恢复自杀标记，然后余额返回账户。
func (self *StateDB) Suicide(addr common.Address) bool {
	// 获取账户的stateObject对象
	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return false
	}
	// 添加删除日志
	self.journal.append(suicideChange{
		account:     &addr,
		prev:        stateObject.suicided,
		prevbalance: new(big.Int).Set(stateObject.Balance()),
	})
	// 标记账户自杀，余额清零
	stateObject.markSuicided()
	stateObject.data.Balance = new(big.Int)

	return true
}

//
// Setting, updating & deleting state object methods.
//

// updateStateObject writes the given object to the trie.
// 把对象RLP编码，然后写到trie
func (self *StateDB) updateStateObject(stateObject *stateObject) {
	addr := stateObject.Address()
	data, err := rlp.EncodeToBytes(stateObject)
	if err != nil {
		panic(fmt.Errorf("can't encode object at %x: %v", addr[:], err))
	}
	self.setError(self.trie.TryUpdate(addr[:], data))
}

// deleteStateObject removes the given object from the state trie.
func (self *StateDB) deleteStateObject(stateObject *stateObject) {
	stateObject.deleted = true
	addr := stateObject.Address()
	self.setError(self.trie.TryDelete(addr[:]))
}

// Retrieve a state object given by the address. Returns nil if not found.
// stateDB中使用trie保存addr到stateObject的映射，stateObject中保存key到value的映射
// 先从stateObjects中读取，否则从Trie读取Account，然后创建stateObject，存到stateObjects
func (self *StateDB) getStateObject(addr common.Address) (stateObject *stateObject) {
	// Prefer 'live' objects.
	if obj := self.stateObjects[addr]; obj != nil {
		if obj.deleted {
			return nil
		}
		return obj
	}

	// Load the object from the database.
	enc, err := self.trie.TryGet(addr[:])
	if len(enc) == 0 {
		self.setError(err)
		return nil
	}
	// trie中实际实际保存的是Account
	var data Account
	if err := rlp.DecodeBytes(enc, &data); err != nil {
		log.Error("Failed to decode state object", "addr", addr, "err", err)
		return nil
	}
	// Insert into the live set.
	obj := newObject(self, addr, data)
	self.setStateObject(obj)
	return obj
}

func (self *StateDB) setStateObject(object *stateObject) {
	self.stateObjects[object.Address()] = object
}

// Retrieve a state object or create a new state object if nil.
// 获取stateObject，不存在则创建
func (self *StateDB) GetOrNewStateObject(addr common.Address) *stateObject {
	stateObject := self.getStateObject(addr)
	if stateObject == nil || stateObject.deleted {
		stateObject, _ = self.createObject(addr)
	}
	return stateObject
}

// createObject creates a new state object. If there is an existing account with
// the given address, it is overwritten and returned as the second return value.
// 创建一个stateObject，对账户数据进行初始化，然后记录日志
func (self *StateDB) createObject(addr common.Address) (newobj, prev *stateObject) {
	// 先看下这个地址之前的账户状态，如果有就不用新建账户
	prev = self.getStateObject(addr)
	newobj = newObject(self, addr, Account{})
	// 设置nonce的初始值
	newobj.setNonce(0) // sets the object to dirty
	if prev == nil {
		// 添加到日志（新的）
		// createObjectChange 注意它的实现
		self.journal.append(createObjectChange{account: &addr})
	} else {
		// 添加到日志
		// resetObjectChange 注意它的实现
		self.journal.append(resetObjectChange{prev: prev})
	}
	// set一个stateObject(新账户-)
	self.setStateObject(newobj)
	return newobj, prev
}

// CreateAccount explicitly creates a state object. If a state object with the address
// already exists the balance is carried over to the new account.
//
// CreateAccount is called during the EVM CREATE operation. The situation might arise that
// a contract does the following:
//
//   1. sends funds to sha(account ++ (nonce + 1))
//   2. tx_create(sha(account ++ nonce)) (note that this gets the address of 1)
//
// Carrying over the balance ensures that Ether doesn't disappear.
// 创建一个新的空账户，如果存在该地址的旧账户，则把旧地址中的余额，放到新账户中
func (self *StateDB) CreateAccount(addr common.Address) {
	new, prev := self.createObject(addr)
	if prev != nil {
		new.setBalance(prev.data.Balance)
	}
}

func (db *StateDB) ForEachStorage(addr common.Address, cb func(key, value common.Hash) bool) {
	so := db.getStateObject(addr)
	if so == nil {
		return
	}
	it := trie.NewIterator(so.getTrie(db.db).NodeIterator(nil))
	for it.Next() {
		key := common.BytesToHash(db.trie.GetKey(it.Key))
		if value, dirty := so.dirtyStorage[key]; dirty {
			cb(key, value)
			continue
		}
		cb(key, common.BytesToHash(it.Value))
	}
}

// Copy creates a deep, independent copy of the state.
// Snapshots of the copied state cannot be applied to the copy.
func (self *StateDB) Copy() *StateDB {
	// Copy all the basic fields, initialize the memory ones
	state := &StateDB{
		db:                self.db,
		trie:              self.db.CopyTrie(self.trie),
		stateObjects:      make(map[common.Address]*stateObject, len(self.journal.dirties)),
		stateObjectsDirty: make(map[common.Address]struct{}, len(self.journal.dirties)),
		refund:            self.refund,
		logs:              make(map[common.Hash][]*types.Log, len(self.logs)),
		logSize:           self.logSize,
		preimages:         make(map[common.Hash][]byte),
		journal:           newJournal(),
	}
	// Copy the dirty states, logs, and preimages
	for addr := range self.journal.dirties {
		// As documented [here](https://github.com/ethereum/go-ethereum/pull/16485#issuecomment-380438527),
		// and in the Finalise-method, there is a case where an object is in the journal but not
		// in the stateObjects: OOG after touch on ripeMD prior to Byzantium. Thus, we need to check for
		// nil
		if object, exist := self.stateObjects[addr]; exist {
			state.stateObjects[addr] = object.deepCopy(state)
			state.stateObjectsDirty[addr] = struct{}{}
		}
	}
	// Above, we don't copy the actual journal. This means that if the copy is copied, the
	// loop above will be a no-op, since the copy's journal is empty.
	// Thus, here we iterate over stateObjects, to enable copies of copies
	for addr := range self.stateObjectsDirty {
		if _, exist := state.stateObjects[addr]; !exist {
			state.stateObjects[addr] = self.stateObjects[addr].deepCopy(state)
			state.stateObjectsDirty[addr] = struct{}{}
		}
	}
	for hash, logs := range self.logs {
		cpy := make([]*types.Log, len(logs))
		for i, l := range logs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		state.logs[hash] = cpy
	}
	for hash, preimage := range self.preimages {
		state.preimages[hash] = preimage
	}
	return state
}

// Snapshot returns an identifier for the current revision of the state.
// 拍摄快照
// 快照只是一个id，把id和日志的长度关联起来，存到Revisions中
// ToView
// 调用时机：
// 1.EVM调用Call、CallCode、DelegateCall、StaticCall、Create的过程中：EVM.Create —> StateDB.Snapshot
// 2.应用交易的过程：Work.commitTransaction —> StateDB.Snapshot
func (self *StateDB) Snapshot() int {
	// 首先获取快照id，从0开始计数
	id := self.nextRevisionId
	self.nextRevisionId++
	// 然后将快照保存，即reversion{id, journal.length}
	self.validRevisions = append(self.validRevisions, revision{id, self.journal.length()})
	return id
}

// RevertToSnapshot reverts all state changes made since the given revision.
// 回滚到指定的快照
// ToView
// 1、检查快照编号是否有效
// 2、通过快照编号获取日志长度
// 3、调用日志中的revert函数进行恢复
// 4、移除恢复点后面的快照
func (self *StateDB) RevertToSnapshot(revid int) {
	// Find the snapshot in the stack of valid snapshots.
	// 找出validReversion[0，n]中最小的下标偏移i，能够满足第二个函数f(i) == true
	idx := sort.Search(len(self.validRevisions), func(i int) bool {
		return self.validRevisions[i].id >= revid
	})
	if idx == len(self.validRevisions) || self.validRevisions[idx].id != revid {
		panic(fmt.Errorf("revision id %v cannot be reverted", revid))
	}
	snapshot := self.validRevisions[idx].journalIndex

	// Replay the journal to undo changes and remove invalidated snapshots
	// 反操作后续的操作，达到回滚的目的
	self.journal.revert(self, snapshot)
	self.validRevisions = self.validRevisions[:idx]
}

// GetRefund returns the current value of the refund counter.
func (self *StateDB) GetRefund() uint64 {
	return self.refund
}

// Finalise和Commit是和存储过程紧密关联的2个函数，Finalise代表修改过的状态已经进入“终态”，Commit代表所有的状态都写入到数据库

// Finalise finalises the state by removing the self destructed objects
// and clears the journal as well as the refunds.
// ToView
// 将状态写进tire中
// 遍历更新的账户，将被更新的账户写入状态树，清除变更日志、快照、返利
// 我们上面分析的所有日志及回滚都是在StateObjects这个map缓存中进行的，一旦这些状态被写进状态树，日志就没用了，不能再回滚了，所以将日志、快照、返利都清除。
// 使用场景：
// 1.执行交易/合约，进行一次状态转移。
// 2.给矿工计算奖励后，进行一次状态转移。
// TODO
func (s *StateDB) Finalise(deleteEmptyObjects bool) {
	// dirties中记录的是变更过的账户
	// 只处理journal中标记为dirty的对象，不处理stateObjectsDirty中的对象
	for addr := range s.journal.dirties {
		// 验证这个账户是否存在于stateObjects列表中，如果不存在就跳过这个账户
		stateObject, exist := s.stateObjects[addr]
		if !exist {
			// ripeMD is 'touched' at block 1714175, in tx 0x1237f737031e40bcde4a8b7e717b2d15e3ecadfe49bb1bbc71ee9deb09c6fcf2
			// That tx goes out of gas, and although the notion of 'touched' does not exist there, the
			// touch-event will still be recorded in the journal. Since ripeMD is a special snowflake,
			// it will persist in the journal even though the journal is reverted. In this special circumstance,
			// it may exist in `s.journal.dirties` but not in `s.stateObjects`.
			// Thus, we can safely ignore it here
			continue
		}
		// 如果账户已经自杀/账户为空且deleteEmptyObjects标志为true，则删除账户(stateobject)
		if stateObject.suicided || (deleteEmptyObjects && stateObject.empty()) {
			s.deleteStateObject(stateObject)
		} else {
			// 否则将账户的storage变更写入storage树
			// 更新树根
			stateObject.updateRoot(s.db)
			// 将当前账户写入状态树
			s.updateStateObject(stateObject)
		}
		// 当账户被更新到状态树后，将改动的账户标记为脏账户
		s.stateObjectsDirty[addr] = struct{}{}
	}
	// Invalidate journal because reverting across transactions is not allowed.
	// 删除过时的日志（此处可以理解为交易的回滚有事务的特征，不能跨交易，就是说这条交易失败了，不能回滚到上上条交易）
	// 清空journal，revision，不能再回滚
	s.clearJournalAndRefund()
}

// IntermediateRoot computes the current root hash of the state trie.
// It is called in between transactions to get the root hash that
// goes into transaction receipts.
// 更新状态树（将状态写入树中），同时计算状态树根
// 调用时机：
// 1.BlockChain验证一个区块的状态树根是否正确：BlockChain.insertChain —>BlockValidator.ValidateState —>stateDB.intermediateRoot。也就是区块插入规范链时，执行完交易后要验证此时本地的状态树树根与发来的区块头中的是否一致
// 2.Worker递交工作的过程中执行全部交易后，需要得到状态树根来填充区块头的root：worker.CommitNewWork —> Ethash.Finalize —> stateDB.IntermeidateRoot。因为挖矿之前要先执行交易，还要结算挖矿奖励，然后生成最新的状态（ApplyTransaction返回交易凭据）。这时候就需要获得状态树树根，放在区块头中，一起打包用于挖矿。
func (s *StateDB) IntermediateRoot(deleteEmptyObjects bool) common.Hash {
	// false
	// 更新状态树
	s.Finalise(deleteEmptyObjects)
	return s.trie.Hash()
}

// Prepare sets the current transaction hash and index and block hash which is
// used when the EVM emits new state logs.
func (self *StateDB) Prepare(thash, bhash common.Hash, ti int) {
	self.thash = thash
	self.bhash = bhash
	self.txIndex = ti
}

func (s *StateDB) clearJournalAndRefund() {
	s.journal = newJournal()
	s.validRevisions = s.validRevisions[:0]
	s.refund = 0
}

// Commit writes the state to the underlying in-memory trie database.
// ToView
// 将状态树tire写入数据库db（即写入区块链），与Finalize不同，这里处理的是Dirty的对象
// 调用时机：
// 1.自己挖到区块。
// 2.收到他人的区块。
func (s *StateDB) Commit(deleteEmptyObjects bool) (root common.Hash, err error) {
	// 清空journal无法再回滚
	defer s.clearJournalAndRefund()
	// 把journal中dirties的对象，加入到stateObjectsDirty
	// 遍历日志脏账户，将被更新的账户写入状态树（因为我们只需要重写那些改动了的账户，没有改动的账户不需要处理）
	for addr := range s.journal.dirties {
		s.stateObjectsDirty[addr] = struct{}{}
	}
	// Commit objects to the trie.
	// 遍历所有活动/修改过的对象
	// 遍历被更新的账户，将被更新的账户写入状态树
	for addr, stateObject := range s.stateObjects {
		_, isDirty := s.stateObjectsDirty[addr]
		switch {
		case stateObject.suicided || (isDirty && deleteEmptyObjects && stateObject.empty()):
			// If the object has been removed, don't bother syncing it
			// and just mark it for deletion in the trie.
			// 如果账户标记为销毁，则从状态树中删除之
			// 如果账户为空，且deleteEmptyObjects标志为true，从状态树中删除账户
			s.deleteStateObject(stateObject)
		case isDirty:
			// Write any contract code associated with the state object
			// tips: stateObject.Code并没有保存在stateObject.trie中，而是保存在stateDB.trie中。
			// 所以调用stateObject.Code获取合约代码的时候，实际传入的是stateDB.db，cachingDB.ContractCode实际也不使用合约的地址，因为(CodeHash, Code)本身就是作为KV存放在Trie中。
			// ..
			// 把修改过的合约代码写到数据库，这个用法高级，直接把数据库拿过来，插进去
			// 注意：这里写入的DB是stateDB的数据库，因为stateObject的Trie只保存Account信息
			// 如果有代码更新，则将code以codeHash为key，以code为value存入db数据库
			if stateObject.code != nil && stateObject.dirtyCode {
				s.db.TrieDB().InsertBlob(common.BytesToHash(stateObject.CodeHash()), stateObject.code)
				stateObject.dirtyCode = false
			}
			// Write any storage changes in the state object to its storage trie.
			// 对象提交：把任何改变的存储数据写到数据库
			if err := stateObject.CommitTrie(s.db); err != nil {
				return common.Hash{}, err
			}
			// Update the object in the main account trie.
			// 把修改后的对象，编码后写入到stateDB的trie中
			// 更新状态树
			s.updateStateObject(stateObject)
		}
		delete(s.stateObjectsDirty, addr)
	}
	// Write trie changes.
	// stateDB的提交
	// 将状态树写入数据库
	root, err = s.trie.Commit(func(leaf []byte, parent common.Hash) error {
		var account Account
		if err := rlp.DecodeBytes(leaf, &account); err != nil {
			return nil
		}
		// 如果叶子节点的trie不空，则trie关联到父节点
		if account.Root != emptyState {
			// TOVIEW
			s.db.TrieDB().Reference(account.Root, parent)
		}
		// 如果叶子节点的code不空（合约账户），则把code关联到父节点
		code := common.BytesToHash(account.CodeHash)
		if code != emptyCode {
			s.db.TrieDB().Reference(code, parent)
		}
		return nil
	})
	log.Debug("Trie cache stats after commit", "misses", trie.CacheMisses(), "unloads", trie.CacheUnloads())
	return root, err
}
