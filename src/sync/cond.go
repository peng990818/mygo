// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
    "sync/atomic"
    "unsafe"
)

// Cond implements a condition variable, a rendezvous point
// for goroutines waiting for or announcing the occurrence
// of an event.
//
// Each Cond has an associated Locker L (often a *Mutex or *RWMutex),
// which must be held when changing the condition and
// when calling the Wait method.
//
// A Cond must not be copied after first use.
//
// In the terminology of the Go memory model, Cond arranges that
// a call to Broadcast or Signal “synchronizes before” any Wait call
// that it unblocks.
//
// For many simple use cases, users will be better off using channels than a
// Cond (Broadcast corresponds to closing a channel, and Signal corresponds to
// sending on a channel).
//
// For more on replacements for sync.Cond, see [Roberto Clapis's series on
// advanced concurrency patterns], as well as [Bryan Mills's talk on concurrency
// patterns].
//
// [Roberto Clapis's series on advanced concurrency patterns]: https://blogtitle.github.io/categories/concurrency/
// [Bryan Mills's talk on concurrency patterns]: https://drive.google.com/file/d/1nPdvhB0PutEJzdCq5ms6UI58dp50fcAN/view
type Cond struct {
    noCopy noCopy

    // L is held while observing or changing the condition
    // 持有的互斥锁
    L Locker

    notify  notifyList  // 通知列表
    checker copyChecker // 拷贝检查器
}

// NewCond returns a new Cond with Locker l.
func NewCond(l Locker) *Cond {
    return &Cond{L: l}
}

// Wait atomically unlocks c.L and suspends execution
// of the calling goroutine. After later resuming execution,
// Wait locks c.L before returning. Unlike in other systems,
// Wait cannot return unless awoken by Broadcast or Signal.
//
// Because c.L is not locked while Wait is waiting, the caller
// typically cannot assume that the condition is true when
// Wait returns. Instead, the caller should Wait in a loop:
//
//	c.L.Lock()
//	for !condition() {
//	    c.Wait()
//	}
//	... make use of condition ...
//	c.L.Unlock()
// Wait 原子式的 unlock c.L， 并暂停执行调用的 goroutine。
// 在稍后执行后，Wait 会在返回前 lock c.L. 与其他系统不同，
// 除非被 Broadcast 或 Signal 唤醒，否则等待无法返回。
//
// 因为等待第一次 resume 时 c.L 没有被锁定，所以当 Wait 返回时，
// 调用者通常不能认为条件为真。相反，调用者应该在循环中使用 Wait()：
//
//    c.L.Lock()
//    for !condition() {
//        c.Wait()
//    }
//    ... make use of condition ...
//    c.L.Unlock()
//
func (c *Cond) Wait() {
    c.checker.check()
    t := runtime_notifyListAdd(&c.notify)
    c.L.Unlock()
    runtime_notifyListWait(&c.notify, t)
    c.L.Lock()
}

// Signal wakes one goroutine waiting on c, if there is any.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
//
// Signal() does not affect goroutine scheduling priority; if other goroutines
// are attempting to lock c.L, they may be awoken before a "waiting" goroutine.
// Signal 唤醒一个等待 c 的 goroutine（如果存在）
//
// 在调用时它可以（不必须）持有一个 c.L
func (c *Cond) Signal() {
    c.checker.check()
    runtime_notifyListNotifyOne(&c.notify)
}

// Broadcast wakes all goroutines waiting on c.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
// Broadcast 唤醒等待 c 的所有 goroutine
//
// 调用时它可以（不必须）持久有个 c.L
func (c *Cond) Broadcast() {
    c.checker.check()
    runtime_notifyListNotifyAll(&c.notify)
}

// copyChecker holds back pointer to itself to detect object copying.
type copyChecker uintptr

func (c *copyChecker) check() {
    // 比较存储在copyChecker中的地址和本身的内存地址，如果不同那么可能被复制了
    // 如果不同，则尝试设置为当前对象的地址。
    // 然后再次进行比较
    if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&
        !atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&
        uintptr(*c) != uintptr(unsafe.Pointer(c)) {
        panic("sync.Cond is copied")
    }
}

// noCopy may be added to structs which must not be copied
// after the first use.
//
// See https://golang.org/issues/8005#issuecomment-190753527
// for details.
//
// Note that it must not be embedded, due to the Lock and Unlock methods.
type noCopy struct{}

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
