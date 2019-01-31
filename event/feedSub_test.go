package event

import (
	"fmt"
	"github.com/PlatONnetwork/PlatON-Go/event"
	"reflect"
	"sync"
	"testing"
)

func TestFeed1(t *testing.T) {
	type someEvent struct{ I int }

	var feed Feed
	var wg sync.WaitGroup

	// 包含事件的channel
	ch := make(chan someEvent)
	// 用Subscribe方法订阅事件通知，需要使用者提前指定接收事件的channel
	sub := feed.Subscribe(ch)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for event := range ch {
			fmt.Printf("Received: %#v\n", event.I)
		}
		sub.Unsubscribe()
		fmt.Println("done")
	}()

	feed.Send(someEvent{5})
	feed.Send(someEvent{10})
	feed.Send(someEvent{7})
	feed.Send(someEvent{14})
	close(ch)

	wg.Wait()
}

func TestFeed2(t *testing.T) {

	var (
		feed   Feed
		recv   sync.WaitGroup
		sender sync.WaitGroup
	)

	ch := make(chan int)
	feed.Subscribe(ch)
	feed.Subscribe(ch)
	feed.Subscribe(ch)

	expectSends := func(value, n int) {
		defer sender.Done()
		if nsent := feed.Send(value); nsent != n {
			fmt.Printf("send delivered %d times, want %d\n", nsent, n)
		}
	}
	expectRecv := func(wantValue, n int) {
		defer recv.Done()
		for v := range ch {
			if v != wantValue {
				fmt.Printf("received %d, want %d\n", v, wantValue)
			} else {
				fmt.Printf("recv v = %d\n", v)
			}
		}
	}

	sender.Add(3)
	for i := 0; i < 3; i++ {
		go expectSends(1, 3)
	}
	go func() {
		sender.Wait()
		close(ch)
	}()
	recv.Add(1)
	go expectRecv(1, 3)
	recv.Wait()
}

func TestFeed3(t *testing.T) {
	ch := make(chan int)
	sub := event.NewSubscription(func(quit <-chan struct{}) error {
		for i := 0; i < 10; i++ {
			select {
			case ch <- i:
			case <-quit:
				fmt.Println("unsubscribed")
				return nil
			}
		}
		return nil
	})

	for i := range ch {
		fmt.Println(i)
		if i == 4 {
			sub.Unsubscribe()
			break
		}
	}
}

func Test1(t *testing.T) {
	aChan := make(chan int, 1)
	chanval := reflect.ValueOf(aChan)
	t.Logf("chanval is %v\n", chanval.String())
	chantyp := chanval.Type()
	if chantyp.Kind() != reflect.Chan || chantyp.ChanDir()&reflect.SendDir == 0 {
		panic(errBadChannel)
	}

	cs := []int{0, 1, 2, 3, 4}
	cs = deactivate(cs, 1)
	for i := 0; i < len(cs); i++ {
		t.Logf("cs %d value is %v\n", i, cs[i])
	}
}

func deactivate(cs []int, index int) []int {
	last := len(cs) - 1
	cs[index], cs[last] = cs[last], cs[index]
	return cs[:last]
}
