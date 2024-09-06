// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
    "internal/race"
    "sync/atomic"
    "unsafe"
)

// Provided by runtime via linkname.
func throw(string)
func fatal(string)

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
//
// In the terminology of the Go memory model,
// the n'th call to Unlock “synchronizes before” the m'th call to Lock
// for any n < m.
// A successful call to TryLock is equivalent to a call to Lock.
// A failed call to TryLock does not establish any “synchronizes before”
// relation at all.
// 互斥锁是一种互斥锁。互斥体的零值是未锁定的互斥体。首次使用后不得复制互斥锁。
// 在 Go 内存模型的术语中，对于任意 n < m，第 n 次 Unlock 调用“同步于”第 m 次 Lock 调用之前。
// 成功调用 TryLock 相当于调用 Lock。对 TryLock 的失败调用根本不会建立任何“同步之前”关系。
type Mutex struct {
    state int32  // 锁当前的状态
    sema  uint32 // 信号量，用于唤醒协程
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
    Lock()
    Unlock()
}

const (
    mutexLocked      = 1 << iota // mutex is locked 锁是否被占用
    mutexWoken                   // 是否有其他协程被唤醒
    mutexStarving                // 当前锁是否处于饥饿模式，饥饿模式下锁会优先传递给等待时间最长的协程
    mutexWaiterShift = iota      // 记录有多少个协程在等待获取锁

    // Mutex fairness.
    //
    // Mutex can be in 2 modes of operations: normal and starvation.
    // In normal mode waiters are queued in FIFO order, but a woken up waiter
    // does not own the mutex and competes with new arriving goroutines over
    // the ownership. New arriving goroutines have an advantage -- they are
    // already running on CPU and there can be lots of them, so a woken up
    // waiter has good chances of losing. In such case it is queued at front
    // of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
    // it switches mutex to the starvation mode.
    //
    // In starvation mode ownership of the mutex is directly handed off from
    // the unlocking goroutine to the waiter at the front of the queue.
    // New arriving goroutines don't try to acquire the mutex even if it appears
    // to be unlocked, and don't try to spin. Instead they queue themselves at
    // the tail of the wait queue.
    //
    // If a waiter receives ownership of the mutex and sees that either
    // (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
    // it switches mutex back to normal operation mode.
    //
    // Normal mode has considerably better performance as a goroutine can acquire
    // a mutex several times in a row even if there are blocked waiters.
    // Starvation mode is important to prevent pathological cases of tail latency.
    starvationThresholdNs = 1e6 // 饥饿阈值
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
    // Fast path: grab unlocked mutex.
    if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
        // 锁在调用时未被占用，则设置为锁定状态
        if race.Enabled {
            // 启用竟态检测，记录锁的恶获取
            race.Acquire(unsafe.Pointer(m))
        }
        return
    }
    // Slow path (outlined so that the fast path can be inlined)
    // 锁已被其他协程持有则进入慢速路径进行处理
    m.lockSlow()
}

// TryLock tries to lock m and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (m *Mutex) TryLock() bool {
    old := m.state
    if old&(mutexLocked|mutexStarving) != 0 {
        return false
    }

    // There may be a goroutine waiting for the mutex, but we are
    // running now and can try to grab the mutex before that
    // goroutine wakes up.
    if !atomic.CompareAndSwapInt32(&m.state, old, old|mutexLocked) {
        return false
    }

    if race.Enabled {
        race.Acquire(unsafe.Pointer(m))
    }
    return true
}

func (m *Mutex) lockSlow() {
    var waitStartTime int64 // 记录开始等待的时间
    starving := false       // 表示当前是否处于饥饿模式
    awoke := false          // 表示当前协程是否已被唤醒
    iter := 0               // 用于记录自旋次数
    old := m.state          // 保存锁的当前状态
    for {
        // Don't spin in starvation mode, ownership is handed off to waiters
        // so we won't be able to acquire the mutex anyway.
        // 锁已被占用但尚未处于饥饿状态并且满足自旋条件
        if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
            // 通过自旋的方式获取锁而不是立即进入阻塞状态
            // Active spinning makes sense.
            // Try to set mutexWoken flag to inform Unlock
            // to not wake other blocked goroutines.
            // mutexWoken未设置并且有等待的协程，则尝试设置mutexWoken，告知后续的解锁操作不要唤醒其他协程
            if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
                atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
                awoke = true
            }
            runtime_doSpin()
            iter++
            old = m.state
            continue
        }
        new := old
        // Don't try to acquire starving mutex, new arriving goroutines must queue.
        // 不要尝试获取一个饥饿的锁，新到来的协程必须在队列中
        if old&mutexStarving == 0 {
            // 锁未处于饥饿状态，当前协程不是在被唤醒后重新尝试获取锁，则将状态更新为已锁住
            new |= mutexLocked
        }
        if old&(mutexLocked|mutexStarving) != 0 {
            // 当前锁已被锁定或正处于饥饿模式，则增加等待协程计数，表示有更多的协程在等待
            new += 1 << mutexWaiterShift
        }
        // The current goroutine switches mutex to starvation mode.
        // But if the mutex is currently unlocked, don't do the switch.
        // Unlock expects that starving mutex has waiters, which will not
        // be true in this case.
        // 饥饿模式处理
        // 当锁进入饥饿模式后，新来的协程将不再尝试自旋获取锁，直接排队等待
        if starving && old&mutexLocked != 0 {
            new |= mutexStarving
        }
        if awoke {
            // The goroutine has been woken from sleep,
            // so we need to reset the flag in either case.
            if new&mutexWoken == 0 {
                throw("sync: inconsistent mutex state")
            }
            new &^= mutexWoken
        }
        if atomic.CompareAndSwapInt32(&m.state, old, new) {
            // 锁获取成功
            if old&(mutexLocked|mutexStarving) == 0 {
                break // locked the mutex with CAS
            }
            // If we were already waiting before, queue at the front of the queue.
            // 当前协程之前就在等待锁了，应该排在等待队列前面
            queueLifo := waitStartTime != 0
            if waitStartTime == 0 {
                // 第一次进入等待状态，则将当前的纳秒时间戳记录，表示从这一刻开始记录等待时间
                waitStartTime = runtime_nanotime()
            }
            // 阻塞等待
            runtime_SemacquireMutex(&m.sema, queueLifo, 1)
            // 检查协程是否进入了饥饿模式，阻塞时间大于1ms或者正处于饥饿模式
            starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
            old = m.state
            if old&mutexStarving != 0 {
                // If this goroutine was woken and mutex is in starvation mode,
                // ownership was handed off to us but mutex is in somewhat
                // inconsistent state: mutexLocked is not set and we are still
                // accounted as waiter. Fix that.
                // 修正锁的状态：如果当前协程被唤醒并处于饥饿模式，那么锁的所有权会被直接交给当前协程。
                // 但此时锁的状态可能不一致，特别是锁的mutexLocked标志位可能没有被重置。
                // 虽然当前协程已经获取了锁，因此需要修正
                // 如果这两个标志位仍然被重置，或者等待计数为0，会触发异常，表示锁的状态不一致
                if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
                    throw("sync: inconsistent mutex state")
                }
                // 修正锁的状态
                // 设置锁定标志位并减少一个等待者引用计数
                delta := int32(mutexLocked - 1<<mutexWaiterShift)
                if !starving || old>>mutexWaiterShift == 1 {
                    // 当前协程没有进入饥饿模式，或者这是最后一个等待者，则解除饥饿模式
                    // Exit starvation mode.
                    // Critical to do it here and consider wait time.
                    // Starvation mode is so inefficient, that two goroutines
                    // can go lock-step infinitely once they switch mutex
                    // to starvation mode.
                    delta -= mutexStarving
                }
                // 修正
                atomic.AddInt32(&m.state, delta)
                break
            }
            awoke = true
            iter = 0
        } else {
            old = m.state
        }
    }

    if race.Enabled {
        race.Acquire(unsafe.Pointer(m))
    }
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
    if race.Enabled {
        _ = m.state
        race.Release(unsafe.Pointer(m))
    }

    // Fast path: drop lock bit.
    // 清除标记，解锁
    new := atomic.AddInt32(&m.state, -mutexLocked)
    if new != 0 {
        // Outlined slow path to allow inlining the fast path.
        // To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
        // 其他协程在等待此锁或其他状态位被设置，因此进入慢速路径处理
        m.unlockSlow(new)
    }
}

func (m *Mutex) unlockSlow(new int32) {
    // 重复解锁判定
    if (new+mutexLocked)&mutexLocked == 0 {
        fatal("sync: unlock of unlocked mutex")
    }
    if new&mutexStarving == 0 {
        // 非饥饿模式
        old := new
        for {
            // If there are no waiters or a goroutine has already
            // been woken or grabbed the lock, no need to wake anyone.
            // In starvation mode ownership is directly handed off from unlocking
            // goroutine to the next waiter. We are not part of this chain,
            // since we did not observe mutexStarving when we unlocked the mutex above.
            // So get off the way.
            if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
                // 没有等待者，锁已经被其他协程获取或者处于饥饿模式，直接返回
                return
            }
            // Grab the right to wake someone.
            // 尝试更新状态
            new = (old - 1<<mutexWaiterShift) | mutexWoken
            if atomic.CompareAndSwapInt32(&m.state, old, new) {
                // 唤醒一个阻塞的协程，而不是唤醒一个等待者
                runtime_Semrelease(&m.sema, false, 1)
                return
            }
            old = m.state
        }
    } else {
        // Starving mode: handoff mutex ownership to the next waiter, and yield
        // our time slice so that the next waiter can start to run immediately.
        // Note: mutexLocked is not set, the waiter will set it after wakeup.
        // But mutex is still considered locked if mutexStarving is set,
        // so new coming goroutines won't acquire it.
        // 饥饿模式下，锁的所有权会直接交给下一个等待者，而不是由刚释放锁的协程继续占有锁
        // 唤醒下一个等待者，也就是等待队列最前端的协程
        runtime_Semrelease(&m.sema, true, 1)
    }
}
