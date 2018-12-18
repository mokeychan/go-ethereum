// Copyright 2016 The go-ethereum Authors
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

package event

import (
	"errors"
	"reflect"
	"sync"
)

var errBadChannel = errors.New("event: Subscribe argument does not have sendable channel type")

// Feed implements one-to-many subscriptions where the carrier of events is a channel.
// Values sent to a Feed are delivered to all subscribed channels simultaneously.
//
// Feed 实现了 1对多的订阅模式，使用了channel来传递事件。 发送给Feed的值会同时被传递给所有订阅的channel。
// Feed 是一个流式事件框架，与其他模块没有耦合。订阅者采用有缓存的通道，Feed就是异步的;采用无缓存的通道，Feed就是同步的。由消息的订阅者决定。
// Feeds can only be used with a single type. The type is determined by the first Send or
// Subscribe operation. Subsequent calls to these methods panic if the type does not
// match.
// 每种事件类型都有一个自己的feed，一个feed内订阅的是同一种类型（同一种类型、同一种类型!）的事件，得用某个事件的feed才能订阅该事件
// Feed只能被单个类型使用。这个和之前的event不同，event可以使用多个类型。 类型被第一个Send调用或者是Subscribe调用决定。 后续的调用如果类型和其不一致会panic
// The zero value is ready to use.
type Feed struct {
	once      sync.Once        // ensures that init only runs once 保证初始化只被执行一次
	sendLock  chan struct{}    // sendLock has a one-element buffer and is empty when held.It protects sendCases. 这个锁确保了只有一个协程在使用go routine
	removeSub chan interface{} // interrupts Send 取消订阅
	sendCases caseList         // the active set of select cases used by Send 发送方的有效case集(订阅的channel列表，这些channel是活跃的)

	// The inbox holds newly subscribed channels until they are added to sendCases.
	mu     sync.Mutex
	inbox  caseList     // 事件列表 selectCase list 不活跃的在这里
	etype  reflect.Type // 事件类型
	closed bool
}

// This is the index of the first actual subscription channel in sendCases.
// sendCases[0] is a SelectRecv case for the removeSub channel.
const firstSubSendCase = 1

type feedTypeError struct {
	got, want reflect.Type
	op        string
}

func (e feedTypeError) Error() string {
	return "event: wrong type in " + e.op + " got " + e.got.String() + ", want " + e.want.String()
}

// 初始化 初始化会被once来保护保证只会被执行一次。
func (f *Feed) init() {
	f.removeSub = make(chan interface{})
	f.sendLock = make(chan struct{}, 1)
	f.sendLock <- struct{}{}
	f.sendCases = caseList{{Chan: reflect.ValueOf(f.removeSub), Dir: reflect.SelectRecv}}
}

// Subscribe adds a channel to the feed. Future sends will be delivered on the channel
// until the subscription is canceled. All channels added must have the same element type.
//
// The channel should have ample buffer space to avoid blocking other subscribers.
// Slow subscribers are not dropped.
// 订阅的方法
func (f *Feed) Subscribe(channel interface{}) Subscription {
	f.once.Do(f.init)

	chanval := reflect.ValueOf(channel)
	chantyp := chanval.Type()
	// 如果类型不是channel 或者是 channel的方向不能发送数据，那么错误退出。
	if chantyp.Kind() != reflect.Chan || chantyp.ChanDir()&reflect.SendDir == 0 {
		panic(errBadChannel)
	}
	// 组装feedSub
	sub := &feedSub{feed: f, channel: chanval, err: make(chan error, 1)}

	f.mu.Lock()
	defer f.mu.Unlock()
	// 只要chan中的event类型与feed不一致，就会panic
	if !f.typecheck(chantyp.Elem()) {
		panic(feedTypeError{op: "Subscribe", got: chantyp, want: reflect.ChanOf(reflect.SendDir, f.etype)})
	}

	// 把通道保存到case
	// Add the select case to the inbox.
	// The next Send will add it to f.sendCases.
	// 根据传入的channel生成了SelectCase，放入inbox
	// 指定了case的发送方向和使用的channel
	cas := reflect.SelectCase{Dir: reflect.SelectSend, Chan: chanval}
	f.inbox = append(f.inbox, cas)
	return sub
}

// note: callers must hold f.mu
func (f *Feed) typecheck(typ reflect.Type) bool {
	if f.etype == nil {
		f.etype = typ
		return true
	}
	return f.etype == typ
}

func (f *Feed) remove(sub *feedSub) {
	// Delete from inbox first, which covers channels
	// that have not been added to f.sendCases yet.
	ch := sub.channel.Interface()
	f.mu.Lock()
	index := f.inbox.find(ch)
	if index != -1 {
		f.inbox = f.inbox.delete(index)
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()

	select {
	case f.removeSub <- ch:
		// Send will remove the channel from f.sendCases.
	case <-f.sendLock:
		// No Send is in progress, delete the channel now that we have the send lock.
		f.sendCases = f.sendCases.delete(f.sendCases.find(ch))
		f.sendLock <- struct{}{}
	}
}

// Send delivers to all subscribed channels simultaneously.
// It returns the number of subscribers that the value was sent to.
// 发布事件的方法
// 同时向所有的订阅者发送事件，返回订阅者的数量
// pending队列中的事件，通知订阅者
// 返回值为消息接收这数量(订阅者数量)
func (f *Feed) Send(value interface{}) (nsent int) {
	rvalue := reflect.ValueOf(value)

	f.once.Do(f.init)
	<-f.sendLock // 获取发送锁

	// Add new cases from the inbox after taking the send lock.
	// 从inbox加入到sendCases，不能订阅的时候直接加入到sendCases，因为可能其他协程在调用发送
	f.mu.Lock()
	f.sendCases = append(f.sendCases, f.inbox...)
	f.inbox = nil

	// 类型检查：如果该feed不是要发送的值的类型，释放锁，并且执行panic
	// 只要chan中的event类型与feed.etype不一致，就会panic
	if !f.typecheck(rvalue.Type()) {
		f.sendLock <- struct{}{} // 释放锁
		panic(feedTypeError{op: "Send", got: rvalue.Type(), want: f.etype})
	}
	f.mu.Unlock()

	// Set the sent value on all channels.
	// 向feed中的每个case/channel发送消息
	for i := firstSubSendCase; i < len(f.sendCases); i++ {
		f.sendCases[i].Send = rvalue
	}

	// Send until all channels except removeSub have been chosen. 'cases' tracks a prefix
	// of sendCases. When a send succeeds, the corresponding case moves to the end of
	// 'cases' and it shrinks by one element.
	// 所有case仍然保留在sendCases，只是用过的会移动到最后面
	cases := f.sendCases
	for {
		// Fast path: try sending without blocking before adding to the select set.
		// This should usually succeed if subscribers are fast enough and have free
		// buffer space.
		// 使用非阻塞式发送，如果不能发送就及时返回
		for i := firstSubSendCase; i < len(cases); i++ {
			// 先尝试着向每个case的chan中去发送数据, 如果不能发送就及时返回
			if cases[i].Chan.TrySend(rvalue) {
				// 如果发送成功
				nsent++ // 订阅者数量增加
				// 把这个case移动到末尾，所以i这个位置就是没处理过的，然后大小减1
				cases = cases.deactivate(i)
				i--
			}
		}
		// 如果这个地方成立，代表所有订阅者都不阻塞，都发送完毕
		if len(cases) == firstSubSendCase {
			break
		}
		// Select on all the receivers, waiting for them to unblock.
		// 返回一个可用的，直到不阻塞。
		chosen, recv, _ := reflect.Select(cases)
		if chosen == 0 /* <-f.removeSub */ {
			// 这个接收方要删除了，删除并缩小sendCases
			index := f.sendCases.find(recv.Interface())
			f.sendCases = f.sendCases.delete(index)
			if index >= 0 && index < len(cases) {
				// Shrink 'cases' too because the removed case was still active.
				cases = f.sendCases[:len(cases)-1]
			}
		} else {
			// reflect已经确保数据已经发送，无需再尝试发送
			cases = cases.deactivate(chosen)
			nsent++
		}
	}

	// 把sendCases中的send都标记为空
	// Forget about the sent value and hand off the send lock.
	for i := firstSubSendCase; i < len(f.sendCases); i++ {
		f.sendCases[i].Send = reflect.Value{}
	}
	f.sendLock <- struct{}{}
	return nsent
}

// 订阅Feed的结构
type feedSub struct {
	feed    *Feed         // 基础Feed
	channel reflect.Value // channel
	errOnce sync.Once
	err     chan error
}

func (sub *feedSub) Unsubscribe() {
	sub.errOnce.Do(func() {
		sub.feed.remove(sub)
		close(sub.err)
	})
}

func (sub *feedSub) Err() <-chan error {
	return sub.err
}

type caseList []reflect.SelectCase

// find returns the index of a case containing the given channel.
func (cs caseList) find(channel interface{}) int {
	for i, cas := range cs {
		if cas.Chan.Interface() == channel {
			return i
		}
	}
	return -1
}

// delete removes the given case from cs.
func (cs caseList) delete(index int) caseList {
	return append(cs[:index], cs[index+1:]...)
}

// deactivate moves the case at index into the non-accessible portion of the cs slice.
// 移动到末尾，数组长度递减
func (cs caseList) deactivate(index int) caseList {
	last := len(cs) - 1
	cs[index], cs[last] = cs[last], cs[index]
	return cs[:last]
}

// func (cs caseList) String() string {
//     s := "["
//     for i, cas := range cs {
//             if i != 0 {
//                     s += ", "
//             }
//             switch cas.Dir {
//             case reflect.SelectSend:
//                     s += fmt.Sprintf("%v<-", cas.Chan.Interface())
//             case reflect.SelectRecv:
//                     s += fmt.Sprintf("<-%v", cas.Chan.Interface())
//             }
//     }
//     return s + "]"
// }
