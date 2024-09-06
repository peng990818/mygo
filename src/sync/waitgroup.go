// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
    "internal/race"
    "sync/atomic"
    "unsafe"
)

// A WaitGroup waits for a collection of goroutines to finish.
// The main goroutine calls Add to set the number of
// goroutines to wait for. Then each of the goroutines
// runs and calls Done when finished. At the same time,
// Wait can be used to block until all goroutines have finished.
//
// A WaitGroup must not be copied after first use.
//
// In the terminology of the Go memory model, a call to Done
// “synchronizes before” the return of any Wait call that it unblocks.
// WaitGroup 用于等待一组 Goroutine 执行完毕。
// 主 Goroutine 调用 Add 来设置需要等待的 Goroutine 的数量
// 然后每个 Goroutine 运行并调用 Done 来确认已经执行网完毕
// 同时，Wait 可以用于阻塞并等待所有 Goroutine 完成。
//
// WaitGroup 在第一次使用后不能被复制
type WaitGroup struct {
    noCopy noCopy

    // 高32位为计数器，低32为等待者计数器
    state atomic.Uint64 // high 32 bits are counter, low 32 bits are waiter count.
    sema  uint32        // 信号量
}

// Add adds delta, which may be negative, to the WaitGroup counter.
// If the counter becomes zero, all goroutines blocked on Wait are released.
// If the counter goes negative, Add panics.
//
// Note that calls with a positive delta that occur when the counter is zero
// must happen before a Wait. Calls with a negative delta, or calls with a
// positive delta that start when the counter is greater than zero, may happen
// at any time.
// Typically this means the calls to Add should execute before the statement
// creating the goroutine or other event to be waited for.
// If a WaitGroup is reused to wait for several independent sets of events,
// new Add calls must happen after all previous Wait calls have returned.
// See the WaitGroup example.
// Add 将 delta（可能为负）加到 WaitGroup 的计数器上
// 如果计数器归零，则所有阻塞在 Wait 的 Goroutine 被释放
// 如果计数器为负，则 panic
//
// 请注意，当计数器为 0 时发生的带有正的 delta 的调用必须在 Wait 之前。
// 当计数器大于 0 时，带有负 delta 的调用或带有正 delta 调用可能在任何时候发生。
// 通常，这意味着 Add 调用必须发生在 Goroutine 创建之前或其他被等待事件之前。
// 如果一个 WaitGroup 被复用于等待几个不同的独立事件集合，必须在前一个 Wait 调用返回后才能调用 Add。
func (wg *WaitGroup) Add(delta int) {
    if race.Enabled {
        if delta < 0 {
            // Synchronize decrements with Wait.
            race.ReleaseMerge(unsafe.Pointer(wg))
        }
        race.Disable()
        defer race.Enable()
    }
    // 计数器追加
    state := wg.state.Add(uint64(delta) << 32)
    // 计数器的值
    v := int32(state >> 32)
    // 等待器的值
    w := uint32(state)
    if race.Enabled && delta > 0 && v == int32(delta) {
        // The first increment must be synchronized with Wait.
        // Need to model this as a read, because there can be
        // several concurrent wg.counter transitions from 0.
        race.Read(unsafe.Pointer(&wg.sema))
    }
    // 如果实际计数器为负，则直接Panic
    if v < 0 {
        panic("sync: negative WaitGroup counter")
    }
    // 在已经调用 Wait 开始等待时，仍然使用 Add 增加计数。这样会导致 WaitGroup 的计数不准确，从而可能导致 goroutine 永远无法退出等待状态，或者其他非预期的行为。
    if w != 0 && delta > 0 && v == int32(delta) {
        panic("sync: WaitGroup misuse: Add called concurrently with Wait")
    }
    // 正常情况直接返回
    if v > 0 || w == 0 {
        return
    }
    // This goroutine has set counter to 0 when waiters > 0.
    // Now there can't be concurrent mutations of state:
    // - Adds must not happen concurrently with Wait,
    // - Wait does not increment waiters if it sees counter == 0.
    // Still do a cheap sanity check to detect WaitGroup misuse.
    // v == 0 w != 0
    // 处理等待的协程
    if wg.state.Load() != state {
        panic("sync: WaitGroup misuse: Add called concurrently with Wait")
    }
    // Reset waiters count to 0.
    wg.state.Store(0)
    // 唤醒所有等待的协程
    for ; w != 0; w-- {
        runtime_Semrelease(&wg.sema, false, 0)
    }
}

// Done decrements the WaitGroup counter by one.
func (wg *WaitGroup) Done() {
    wg.Add(-1)
}

// Wait blocks until the WaitGroup counter is zero.
func (wg *WaitGroup) Wait() {
    if race.Enabled {
        race.Disable()
    }
    for {
        // 加载当前状态
        state := wg.state.Load()
        // 计数器数量
        v := int32(state >> 32)
        // 等待器数量
        w := uint32(state)
        // 计数器为0，说明所有的协程都已经完成，当前的Wait调用可以直接返回
        if v == 0 {
            // Counter is 0, no need to wait.
            if race.Enabled {
                race.Enable()
                race.Acquire(unsafe.Pointer(wg))
            }
            return
        }
        // Increment waiters count.
        // 增加等待者计数
        if wg.state.CompareAndSwap(state, state+1) {
            if race.Enabled && w == 0 {
                // Wait must be synchronized with the first Add.
                // Need to model this is as a write to race with the read in Add.
                // As a consequence, can do the write only for the first waiter,
                // otherwise concurrent Waits will race with each other.
                race.Write(unsafe.Pointer(&wg.sema))
            }
            // 阻塞等待
            runtime_Semacquire(&wg.sema)
            // 如果计数器在Wait返回之前没有恢复到0，则panic
            // WaitGroup被误用导致Wait没有返回之前再次使用Add
            if wg.state.Load() != 0 {
                panic("sync: WaitGroup is reused before previous Wait has returned")
            }
            if race.Enabled {
                race.Enable()
                race.Acquire(unsafe.Pointer(wg))
            }
            return
        }
    }
}
