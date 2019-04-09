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

// Package miner implements Ethereum block creation and mining.
package miner

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// Backend wraps all methods required for mining.
type Backend interface {
	BlockChain() *core.BlockChain
	TxPool() *core.TxPool
}

// Miner creates blocks and searches for proof-of-work values.
type Miner struct {
	mux      *event.TypeMux   // 事件锁
	worker   *worker          // worker模块，用于支持主要的挖矿流程
	coinbase common.Address   // 矿工地址
	eth      Backend          // Backend对象，Backend是一个自定义接口封装了所有挖矿所需方法(以太坊命令终端)
	engine   consensus.Engine // 共识引擎
	exitCh   chan struct{}    // 停止挖矿的channel

	// 两个调控Miner模块是否运行的开关
	canStart    int32 // can start indicates whether we can start the mining operation
	shouldStart int32 // should start indicates whether we should start after sync  0 为关闭
}

// 挖矿开始前需要创建新的miner
func New(eth Backend, config *params.ChainConfig, mux *event.TypeMux, engine consensus.Engine, recommit time.Duration, gasFloor, gasCeil uint64, isLocalBlock func(block *types.Block) bool) *Miner {
	miner := &Miner{
		eth:    eth,
		mux:    mux,
		engine: engine,
		exitCh: make(chan struct{}),
		// 创建真正挖矿的苦命仔worker
		worker:   newWorker(config, engine, eth, mux, recommit, gasFloor, gasCeil, isLocalBlock),
		canStart: 1,
	}
	// 启动一个线程执行update，首先订阅了downloader的相关的几个事件。
	/*可以看到如果当前处于 区块的同步中，则挖矿的操作需要停止，
	直到同步操作结束（同步成功或是失败），
	如果原来已经执行了挖矿操作的，则继续开启挖矿操作*/
	go miner.update()

	return miner
}

// update keeps track of the downloader events. Please be aware that this is a one shot type of update loop.
// It's entered once and as soon as `Done` or `Failed` has been broadcasted the events are unregistered and
// the loop is exited. This to prevent a major security vuln where external parties can DOS you with blocks
// and halt your mining operation for as long as the DOS continues.
// 监听downloader事件，控制着canStart和shouldStart这两个开关，用于抵挡DOS攻击
// 收到Downloader的StartEvent时，意味者此时本节点正在从其他节点下载新区块，这时miner会立即停止进行中的挖掘工作，并继续监听；如果收到DoneEvent或FailEvent时，意味本节点的下载任务已结束-无论下载成功或失败-此时都可以开始挖掘新区块，并且此时会退出Downloader事件的监听。
// 也就是说，只要接收到来自其他节点的区块，本节点就会自动停止当前区块的挖掘，转向下一个区块。
func (self *Miner) update() {
	// 注册下载开始事件，下载结束事件，下载失败事件
	events := self.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})
	// 处理完以后要取消订阅
	defer events.Unsubscribe()

	for {
		select {
		case ev := <-events.Chan():
			if ev == nil {
				return
			}
			switch ev.Data.(type) {
			// 当监听到downloader的StartEvent事件时，canStart设置为0，表示downloader同步时不可进行挖矿
			case downloader.StartEvent:
				atomic.StoreInt32(&self.canStart, 0)
				// 如果正在挖矿，停止挖矿，同时将shouldStart设置为1，以便下次直接开始挖矿
				if self.Mining() {
					self.Stop()
					atomic.StoreInt32(&self.shouldStart, 1)
					log.Info("Mining aborted due to sync")
				}
			// 当监听到downloader的DoneEvent事件或者FailedEvent事件
			case downloader.DoneEvent, downloader.FailedEvent:
				// 判断shouldStart是否打开
				shouldStart := atomic.LoadInt32(&self.shouldStart) == 1

				atomic.StoreInt32(&self.canStart, 1)
				atomic.StoreInt32(&self.shouldStart, 0)
				// 如果是打开的，则再打开canStart，将shouldStart关闭
				if shouldStart {
					// 将挖矿的控制权完全交给miner.Start()方法
					self.Start(self.coinbase)
				}
				// stop immediately and ignore all further pending events
				return
			}
		// 收到停止挖矿的信号就直接退出
		case <-self.exitCh:
			return
		}
	}
}

// miner的启动，打开shouldStart，设置coinbase，然后启动worker
func (self *Miner) Start(coinbase common.Address) {
	atomic.StoreInt32(&self.shouldStart, 1)
	self.SetEtherbase(coinbase)

	if atomic.LoadInt32(&self.canStart) == 0 {
		log.Info("Network syncing, will start miner afterwards")
		return
	}
	self.worker.start()
}

func (self *Miner) Stop() {
	self.worker.stop()
	// 停止挖矿的时候将模块开关调为0
	atomic.StoreInt32(&self.shouldStart, 0)
}

func (self *Miner) Close() {
	self.worker.close()
	close(self.exitCh)
}

func (self *Miner) Mining() bool {
	return self.worker.isRunning()
}

func (self *Miner) HashRate() uint64 {
	if pow, ok := self.engine.(consensus.PoW); ok {
		return uint64(pow.Hashrate())
	}
	return 0
}

func (self *Miner) SetExtra(extra []byte) error {
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("Extra exceeds max length. %d > %v", len(extra), params.MaximumExtraDataSize)
	}
	self.worker.setExtra(extra)
	return nil
}

// SetRecommitInterval sets the interval for sealing work resubmitting.
func (self *Miner) SetRecommitInterval(interval time.Duration) {
	self.worker.setRecommitInterval(interval)
}

// Pending returns the currently pending block and associated state.
func (self *Miner) Pending() (*types.Block, *state.StateDB) {
	return self.worker.pending()
}

// PendingBlock returns the currently pending block.
//
// Note, to access both the pending block and the pending state
// simultaneously, please use Pending(), as the pending state can
// change between multiple method calls
func (self *Miner) PendingBlock() *types.Block {
	return self.worker.pendingBlock()
}

func (self *Miner) SetEtherbase(addr common.Address) {
	self.coinbase = addr
	self.worker.setEtherbase(addr)
}
