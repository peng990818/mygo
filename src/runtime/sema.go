// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Semaphore implementation exposed to Go.
// Intended use is provide a sleep and wakeup
// primitive that can be used in the contended case
// of other synchronization primitives.
// Thus it targets the same goal as Linux's futex,
// but it has much simpler semantics.
//
// That is, don't think of these as semaphores.
// Think of them as a way to implement sleep and wakeup
// such that every sleep is paired with a single wakeup,
// even if, due to races, the wakeup happens before the sleep.
//
// See Mullender and Cox, ``Semaphores in Plan 9,''
// https://swtch.com/semaphore.pdf

package runtime

import (
    "internal/cpu"
    "runtime/internal/atomic"
    "unsafe"
)

// todo 树堆treap待研究
// Asynchronous semaphore for sync.Mutex.

// A semaRoot holds a balanced tree of sudog with distinct addresses (s.elem).
// Each of those sudog may in turn point (through s.waitlink) to a list
// of other sudogs waiting on the same address.
// The operations on the inner lists of sudogs with the same address
// are all O(1). The scanning of the top-level semaRoot list is O(log n),
// where n is the number of distinct addresses with goroutines blocked
// on them that hash to the given semaRoot.
// See golang.org/issue/17953 for a program that worked badly
// before we introduced the second level of list, and
// BenchmarkSemTable/OneAddrCollision/* for a benchmark that exercises this.
type semaRoot struct {
    // lock 锁
    // treap 树堆 存等待的g tree-heap
    // nwait 等待者的个数
    lock  mutex
    treap *sudog        // root of balanced tree of unique waiters.
    nwait atomic.Uint32 // Number of waiters. Read w/o the lock.
}

var semtable semTable

// Prime to not correlate with any user patterns.
const semTabSize = 251

// 信号表
type semTable [semTabSize]struct {
    root semaRoot
    pad  [cpu.CacheLinePadSize - unsafe.Sizeof(semaRoot{})]byte
}

func (t *semTable) rootFor(addr *uint32) *semaRoot {
    return &t[(uintptr(unsafe.Pointer(addr))>>3)%semTabSize].root
}

//go:linkname sync_runtime_Semacquire sync.runtime_Semacquire
func sync_runtime_Semacquire(addr *uint32) {
    semacquire1(addr, false, semaBlockProfile, 0, waitReasonSemacquire)
}

//go:linkname poll_runtime_Semacquire internal/poll.runtime_Semacquire
func poll_runtime_Semacquire(addr *uint32) {
    semacquire1(addr, false, semaBlockProfile, 0, waitReasonSemacquire)
}

//go:linkname sync_runtime_Semrelease sync.runtime_Semrelease
func sync_runtime_Semrelease(addr *uint32, handoff bool, skipframes int) {
    semrelease1(addr, handoff, skipframes)
}

//go:linkname sync_runtime_SemacquireMutex sync.runtime_SemacquireMutex
func sync_runtime_SemacquireMutex(addr *uint32, lifo bool, skipframes int) {
    semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncMutexLock)
}

//go:linkname sync_runtime_SemacquireRWMutexR sync.runtime_SemacquireRWMutexR
func sync_runtime_SemacquireRWMutexR(addr *uint32, lifo bool, skipframes int) {
    semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncRWMutexRLock)
}

//go:linkname sync_runtime_SemacquireRWMutex sync.runtime_SemacquireRWMutex
func sync_runtime_SemacquireRWMutex(addr *uint32, lifo bool, skipframes int) {
    semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncRWMutexLock)
}

//go:linkname poll_runtime_Semrelease internal/poll.runtime_Semrelease
func poll_runtime_Semrelease(addr *uint32) {
    semrelease(addr)
}

func readyWithTime(s *sudog, traceskip int) {
    if s.releasetime != 0 {
        s.releasetime = cputicks()
    }
    goready(s.g, traceskip)
}

type semaProfileFlags int

const (
    semaBlockProfile semaProfileFlags = 1 << iota // 阻塞事件
    semaMutexProfile                              // 锁事件
)

// Called from runtime.
func semacquire(addr *uint32) {
    semacquire1(addr, false, 0, 0, waitReasonSemacquire)
}

// 将当期协程存放入树堆中，等待被唤醒
func semacquire1(addr *uint32, lifo bool, profile semaProfileFlags, skipframes int, reason waitReason) {
    gp := getg()
    if gp != gp.m.curg {
        throw("semacquire not on the G stack")
    }

    // Easy case.
    // 如果有正在执行 semrelease1 直接获取信号量 返回
    if cansemacquire(addr) {
        return
    }

    // Harder case:
    //	increment waiter count
    //	try cansemacquire one more time, return if succeeded
    //	enqueue itself as a waiter
    //	sleep
    //	(waiter descriptor is dequeued by signaler)
    s := acquireSudog()
    root := semtable.rootFor(addr)
    t0 := int64(0)
    s.releasetime = 0
    s.acquiretime = 0
    s.ticket = 0
    if profile&semaBlockProfile != 0 && blockprofilerate > 0 {
        // 阻塞事件
        t0 = cputicks()
        // 阻塞没有释放时间
        s.releasetime = -1
    }
    if profile&semaMutexProfile != 0 && mutexprofilerate > 0 {
        // 锁事件
        if t0 == 0 {
            t0 = cputicks()
        }
        // 记录获取时间
        s.acquiretime = t0
    }
    for {
        lockWithRank(&root.lock, lockRankRoot)
        // Add ourselves to nwait to disable "easy case" in semrelease.
        // 添加root的等待者
        root.nwait.Add(1)
        // Check cansemacquire to avoid missed wakeup.
        // 再次检测是否有正在释放的信号量
        if cansemacquire(addr) {
            root.nwait.Add(-1)
            unlock(&root.lock)
            break
        }
        // Any semrelease after the cansemacquire knows we're waiting
        // (we set nwait above), so go to sleep.
        // 入队 休眠
        root.queue(addr, s, lifo)
        // 等待被唤醒
        goparkunlock(&root.lock, reason, traceEvGoBlockSync, 4+skipframes)
        if s.ticket != 0 || cansemacquire(addr) {
            // 已经获取过 或 addr 还可以获取 sema
            break
        }
    }
    // 被唤醒后是否记录阻塞事件
    if s.releasetime > 0 {
        // 尝试记录阻塞事件
        blockevent(s.releasetime-t0, 3+skipframes)
    }
    releaseSudog(s)
}

func semrelease(addr *uint32) {
    semrelease1(addr, false, 0)
}

// 唤醒等待协程
// 解锁单个 addr 的等待 g 并唤醒该 g
func semrelease1(addr *uint32, handoff bool, skipframes int) {
    root := semtable.rootFor(addr)
    // 增加信号量
    atomic.Xadd(addr, 1)

    // Easy case: no waiters?
    // This check must happen after the xadd, to avoid a missed wakeup
    // (see loop in semacquire).
    // 检测操作必须在 xadd 后面
    // 避免错过唤醒
    // 这边加上 那边获取
    if root.nwait.Load() == 0 {
        // 没有等待的 g 直接返回
        return
    }

    // Harder case: search for a waiter and wake it.
    lockWithRank(&root.lock, lockRankRoot)
    if root.nwait.Load() == 0 {
        // The count is already consumed by another goroutine,
        // so no need to wake up another goroutine.
        // 二次检测
        unlock(&root.lock)
        return
    }
    // 从树中弹出一个 sudog
    s, t0 := root.dequeue(addr)
    if s != nil {
        // 弹出有效
        root.nwait.Add(-1)
    }
    unlock(&root.lock)
    if s != nil { // May be slow or even yield, so unlock first
        acquiretime := s.acquiretime
        if acquiretime != 0 {
            // 有 acquiretime 表示是锁事件 尝试记录锁事件
            mutexevent(t0-acquiretime, 3+skipframes)
        }
        if s.ticket != 0 {
            throw("corrupted semaphore ticket")
        }
        if handoff && cansemacquire(addr) {
            // 只有外部需要让出调度时才会获取信号量
            // 标记下次执行 并且 addr还可以被获取
            s.ticket = 1
        }
        // 唤醒s 加入就绪队列
        readyWithTime(s, 5+skipframes)
        if s.ticket == 1 && getg().m.locks == 0 {
            // Direct G handoff
            // readyWithTime has added the waiter G as runnext in the
            // current P; we now call the scheduler so that we start running
            // the waiter G immediately.
            // Note that waiter inherits our time slice: this is desirable
            // to avoid having a highly contended semaphore hog the P
            // indefinitely. goyield is like Gosched, but it emits a
            // "preempted" trace event instead and, more importantly, puts
            // the current G on the local runq instead of the global one.
            // We only do this in the starving regime (handoff=true), as in
            // the non-starving case it is possible for a different waiter
            // to acquire the semaphore while we are yielding/scheduling,
            // and this would be wasteful. We wait instead to enter starving
            // regime, and then we start to do direct handoffs of ticket and
            // P.
            // See issue 33747 for discussion.
            // 标记为1 并且 m 没有被其他 g 锁住
            // 让出 m 等待下次调度
            // 即直接调度 s 对应的 g
            // 请注意，继承了我们的时间片：这是可取的，以避免一个高度竞争的信号量无限期地占用 P
            // goyield 类似于 Gosched，但它会发出一个“抢占式”跟踪事件
            // 更重要的是，将当前 G 放在本地 runq 而不是全局 runq 上
            // 我们只在饥饿状态下执行此操作（handoff=true）
            // 因为在非饥饿情况下，在我们让出调度时，其他服务员可能会获取信号量，这将是一种浪费
            // 相反，我们等待进入饥饿状态，然后我们开始直接切换票和 P
            goyield()
        }
    }
}

func cansemacquire(addr *uint32) bool {
    for {
        v := atomic.Load(addr)
        if v == 0 {
            return false
        }
        if atomic.Cas(addr, v, v-1) {
            return true
        }
    }
}

// queue adds s to the blocked goroutines in semaRoot.
func (root *semaRoot) queue(addr *uint32, s *sudog, lifo bool) {
    s.g = getg()
    s.elem = unsafe.Pointer(addr)
    s.next = nil
    s.prev = nil

    var last *sudog
    pt := &root.treap
    for t := *pt; t != nil; t = *pt {
        if t.elem == unsafe.Pointer(addr) {
            // Already have addr in list.
            if lifo {
                // Substitute s in t's place in treap.
                *pt = s
                s.ticket = t.ticket
                s.acquiretime = t.acquiretime
                s.parent = t.parent
                s.prev = t.prev
                s.next = t.next
                if s.prev != nil {
                    s.prev.parent = s
                }
                if s.next != nil {
                    s.next.parent = s
                }
                // Add t first in s's wait list.
                s.waitlink = t
                s.waittail = t.waittail
                if s.waittail == nil {
                    s.waittail = t
                }
                t.parent = nil
                t.prev = nil
                t.next = nil
                t.waittail = nil
            } else {
                // Add s to end of t's wait list.
                if t.waittail == nil {
                    t.waitlink = s
                } else {
                    t.waittail.waitlink = s
                }
                t.waittail = s
                s.waitlink = nil
            }
            return
        }
        last = t
        if uintptr(unsafe.Pointer(addr)) < uintptr(t.elem) {
            pt = &t.prev
        } else {
            pt = &t.next
        }
    }

    // Add s as new leaf in tree of unique addrs.
    // The balanced tree is a treap using ticket as the random heap priority.
    // That is, it is a binary tree ordered according to the elem addresses,
    // but then among the space of possible binary trees respecting those
    // addresses, it is kept balanced on average by maintaining a heap ordering
    // on the ticket: s.ticket <= both s.prev.ticket and s.next.ticket.
    // https://en.wikipedia.org/wiki/Treap
    // https://faculty.washington.edu/aragon/pubs/rst89.pdf
    //
    // s.ticket compared with zero in couple of places, therefore set lowest bit.
    // It will not affect treap's quality noticeably.
    s.ticket = fastrand() | 1
    s.parent = last
    *pt = s

    // Rotate up into tree according to ticket (priority).
    for s.parent != nil && s.parent.ticket > s.ticket {
        if s.parent.prev == s {
            root.rotateRight(s.parent)
        } else {
            if s.parent.next != s {
                panic("semaRoot queue")
            }
            root.rotateLeft(s.parent)
        }
    }
}

// dequeue searches for and finds the first goroutine
// in semaRoot blocked on addr.
// If the sudog was being profiled, dequeue returns the time
// at which it was woken up as now. Otherwise now is 0.
func (root *semaRoot) dequeue(addr *uint32) (found *sudog, now int64) {
    ps := &root.treap
    s := *ps
    for ; s != nil; s = *ps {
        if s.elem == unsafe.Pointer(addr) {
            goto Found
        }
        if uintptr(unsafe.Pointer(addr)) < uintptr(s.elem) {
            ps = &s.prev
        } else {
            ps = &s.next
        }
    }
    return nil, 0

Found:
    now = int64(0)
    if s.acquiretime != 0 {
        now = cputicks()
    }
    if t := s.waitlink; t != nil {
        // Substitute t, also waiting on addr, for s in root tree of unique addrs.
        *ps = t
        t.ticket = s.ticket
        t.parent = s.parent
        t.prev = s.prev
        if t.prev != nil {
            t.prev.parent = t
        }
        t.next = s.next
        if t.next != nil {
            t.next.parent = t
        }
        if t.waitlink != nil {
            t.waittail = s.waittail
        } else {
            t.waittail = nil
        }
        t.acquiretime = now
        s.waitlink = nil
        s.waittail = nil
    } else {
        // Rotate s down to be leaf of tree for removal, respecting priorities.
        for s.next != nil || s.prev != nil {
            if s.next == nil || s.prev != nil && s.prev.ticket < s.next.ticket {
                root.rotateRight(s)
            } else {
                root.rotateLeft(s)
            }
        }
        // Remove s, now a leaf.
        if s.parent != nil {
            if s.parent.prev == s {
                s.parent.prev = nil
            } else {
                s.parent.next = nil
            }
        } else {
            root.treap = nil
        }
    }
    s.parent = nil
    s.elem = nil
    s.next = nil
    s.prev = nil
    s.ticket = 0
    return s, now
}

// rotateLeft rotates the tree rooted at node x.
// turning (x a (y b c)) into (y (x a b) c).
func (root *semaRoot) rotateLeft(x *sudog) {
    // p -> (x a (y b c))
    p := x.parent
    y := x.next
    b := y.prev

    y.prev = x
    x.parent = y
    x.next = b
    if b != nil {
        b.parent = x
    }

    y.parent = p
    if p == nil {
        root.treap = y
    } else if p.prev == x {
        p.prev = y
    } else {
        if p.next != x {
            throw("semaRoot rotateLeft")
        }
        p.next = y
    }
}

// rotateRight rotates the tree rooted at node y.
// turning (y (x a b) c) into (x a (y b c)).
func (root *semaRoot) rotateRight(y *sudog) {
    // p -> (y (x a b) c)
    p := y.parent
    x := y.prev
    b := x.next

    x.next = y
    y.parent = x
    y.prev = b
    if b != nil {
        b.parent = y
    }

    x.parent = p
    if p == nil {
        root.treap = x
    } else if p.prev == y {
        p.prev = x
    } else {
        if p.next != y {
            throw("semaRoot rotateRight")
        }
        p.next = x
    }
}

// notifyList is a ticket-based notification list used to implement sync.Cond.
//
// It must be kept in sync with the sync package.
// 用于 sync.Cond 的通知列表
// wait 为下一个等待者序号
// notify 为已经通知过的等待者序号
type notifyList struct {
    // wait is the ticket number of the next waiter. It is atomically
    // incremented outside the lock.
    // wait 为下一个 waiter 的 ticket 编号
    // 在没有 lock 的情况下原子自增
    wait atomic.Uint32

    // notify is the ticket number of the next waiter to be notified. It can
    // be read outside the lock, but is only written to with lock held.
    //
    // Both wait & notify can wrap around, and such cases will be correctly
    // handled as long as their "unwrapped" difference is bounded by 2^31.
    // For this not to be the case, we'd need to have 2^31+ goroutines
    // blocked on the same condvar, which is currently not possible.
    //
    // notify 是下一个被通知的 waiter 的 ticket 编号
    // 它可以在没有 lock 的情况下进行读取，但只有在持有 lock 的情况下才能进行写
    //
    // wait 和 notify 会产生 wrap around，只要它们 "unwrapped"
    // 的差别小于 2^31，这种情况可以被正确处理。对于 wrap around 的情况而言，
    // 我们需要超过 2^31+ 个 goroutine 阻塞在相同的 condvar 上，这是不可能的。
    //
    // 下一个被通知的等待者序号 可以不用锁读但是必须锁写
    // wait 和 notify 可能会重复 但目前不太可能
    notify uint32

    // List of parked waiters.
    // waiter 列表.
    lock mutex
    head *sudog
    tail *sudog
}

// less checks if a < b, considering a & b running counts that may overflow the
// 32-bit range, and that their "unwrapped" difference is always less than 2^31.
func less(a, b uint32) bool {
    return int32(a-b) < 0
}

// notifyListAdd adds the caller to a notify list such that it can receive
// notifications. The caller must eventually call notifyListWait to wait for
// such a notification, passing the returned ticket number.
//
// notifyListAdd 将调用者添加到通知列表，以便接收通知。
// 调用者最终必须调用 notifyListWait 等待这样的通知，并传递返回的 ticket 编号。
//go:linkname notifyListAdd sync.runtime_notifyListAdd
func notifyListAdd(l *notifyList) uint32 {
    // This may be called concurrently, for example, when called from
    // sync.Cond.Wait while holding a RWMutex in read mode.
    // 这可以并发调用，例如，当在 read 模式下保持 RWMutex 时从 sync.Cond.Wait 调用时。
    return l.wait.Add(1) - 1
}

// notifyListWait waits for a notification. If one has been sent since
// notifyListAdd was called, it returns immediately. Otherwise, it blocks.
//
// notifyListWait 等待通知。如果在调用 notifyListAdd 后发送了一个，则立即返回。否则，它会阻塞。
//go:linkname notifyListWait sync.runtime_notifyListWait
func notifyListWait(l *notifyList, t uint32) {
    // 锁定通知列表，确保并发安全性
    lockWithRank(&l.lock, lockRankNotifyList)

    // Return right away if this ticket has already been notified.
    // 如果 ticket 编号对应的 goroutine 已经被通知到，则立刻返回
    if less(t, l.notify) {
        unlock(&l.lock)
        return
    }

    // Enqueue itself.
    // 将当前协程加入等待队列
    s := acquireSudog()
    s.g = getg()
    s.ticket = t
    s.releasetime = 0
    t0 := int64(0)
    if blockprofilerate > 0 {
        t0 = cputicks()
        s.releasetime = -1
    }
    // 更新等待队列
    if l.tail == nil {
        l.head = s
    } else {
        l.tail.next = s
    }
    l.tail = s
    // 挂起协程并释放锁，等待被唤醒
    goparkunlock(&l.lock, waitReasonSyncCondWait, traceEvGoBlockCond, 3)
    // 唤醒后释放sudog
    if t0 != 0 {
        blockevent(s.releasetime-t0, 2)
    }
    releaseSudog(s)
}

// notifyListNotifyAll notifies all entries in the list.
//
//go:linkname notifyListNotifyAll sync.runtime_notifyListNotifyAll
func notifyListNotifyAll(l *notifyList) {
    // Fast-path: if there are no new waiters since the last notification
    // we don't need to acquire the lock.
    if l.wait.Load() == atomic.Load(&l.notify) {
        return
    }

    // Pull the list out into a local variable, waiters will be readied
    // outside the lock.
    lockWithRank(&l.lock, lockRankNotifyList)
    s := l.head
    l.head = nil
    l.tail = nil

    // Update the next ticket to be notified. We can set it to the current
    // value of wait because any previous waiters are already in the list
    // or will notice that they have already been notified when trying to
    // add themselves to the list.
    atomic.Store(&l.notify, l.wait.Load())
    unlock(&l.lock)

    // Go through the local list and ready all waiters.
    for s != nil {
        next := s.next
        s.next = nil
        readyWithTime(s, 4)
        s = next
    }
}

// notifyListNotifyOne notifies one entry in the list.
//
// 通知列表中的一个条目
//go:linkname notifyListNotifyOne sync.runtime_notifyListNotifyOne
func notifyListNotifyOne(l *notifyList) {
    // Fast-path: if there are no new waiters since the last notification
    // we don't need to acquire the lock at all.
    // 快速路径检查：函数检查自上次通知以来是否有新的等待者，如果wait和notify的值相等
    // 表示没有等待者，无需继续执行操作
    if l.wait.Load() == atomic.Load(&l.notify) {
        return
    }

    lockWithRank(&l.lock, lockRankNotifyList)

    // Re-check under the lock if we need to do anything.
    // 锁内重排检查，在获取锁后，二次检查两者的值，如果相等则还是没有需要通知的等待者
    t := l.notify
    if t == l.wait.Load() {
        unlock(&l.lock)
        return
    }

    // Update the next notify ticket number.
    // 更新通知序号
    atomic.Store(&l.notify, t+1)

    // Try to find the g that needs to be notified.
    // If it hasn't made it to the list yet we won't find it,
    // but it won't park itself once it sees the new notify number.
    //
    // This scan looks linear but essentially always stops quickly.
    // Because g's queue separately from taking numbers,
    // there may be minor reorderings in the list, but we
    // expect the g we're looking for to be near the front.
    // The g has others in front of it on the list only to the
    // extent that it lost the race, so the iteration will not
    // be too long. This applies even when the g is missing:
    // it hasn't yet gotten to sleep and has lost the race to
    // the (few) other g's that we find on the list.
    // 尝试找到需要通知的sudog
    for p, s := (*sudog)(nil), l.head; s != nil; p, s = s, s.next {
        if s.ticket == t {
            // 在列表中移除
            n := s.next
            if p != nil {
                p.next = n
            } else {
                l.head = n
            }
            if n == nil {
                l.tail = p
            }
            unlock(&l.lock)
            s.next = nil
            readyWithTime(s, 4)
            return
        }
    }
    unlock(&l.lock)
}

//go:linkname notifyListCheck sync.runtime_notifyListCheck
func notifyListCheck(sz uintptr) {
    if sz != unsafe.Sizeof(notifyList{}) {
        print("runtime: bad notifyList size - sync=", sz, " runtime=", unsafe.Sizeof(notifyList{}), "\n")
        throw("bad notifyList size")
    }
}

//go:linkname sync_nanotime sync.runtime_nanotime
func sync_nanotime() int64 {
    return nanotime()
}
