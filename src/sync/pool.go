// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
    "internal/race"
    "runtime"
    "sync/atomic"
    "unsafe"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// A Pool must not be copied after first use.
//
// In the terminology of the Go memory model, a call to Put(x) “synchronizes before”
// a call to Get returning that same value x.
// Similarly, a call to New returning x “synchronizes before”
// a call to Get returning that same value x.
type Pool struct {
    noCopy noCopy

    // local 固定大小 per-P 数组, 实际类型为 [P]poolLocal
    local     unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal
    localSize uintptr        // size of the local array local array 的大小

    // 来自前一个垃圾回收周期的 local
    victim     unsafe.Pointer // local from previous cycle
    victimSize uintptr        // size of victims array victim 数组的大小

    // New optionally specifies a function to generate
    // a value when Get would otherwise return nil.
    // It may not be changed concurrently with calls to Get.
    New func() any
}

// Local per-P Pool appendix.
type poolLocalInternal struct {
    // 只能被一个P读写
    // private用于快速访问上次放入的对象
    private any // Can be used only by the respective P.
    // 可以在多个P之间共享读写
    shared poolChain // Local P can pushHead/popHead; any P can popTail.
}

// 每个poolLocal都只被一个P拥有
type poolLocal struct {
    poolLocalInternal

    // Prevents false sharing on widespread platforms with
    // 128 mod (cache line size) = 0 .
    pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrandn(n uint32) uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x any) unsafe.Pointer {
    ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
    h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
    return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put adds x to the pool.
func (p *Pool) Put(x any) {
    if x == nil {
        return
    }
    if race.Enabled {
        if fastrandn(4) == 0 {
            // Randomly drop x on floor.
            return
        }
        race.ReleaseMerge(poolRaceAddr(x))
        race.Disable()
    }
    // 获取一个poolLocal
    l, _ := p.pin()
    // 优先放入private
    if l.private == nil {
        l.private = x
    } else {
        // 不能则放入shared头部
        l.shared.pushHead(x)
    }
    runtime_procUnpin()
    if race.Enabled {
        race.Enable()
    }
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
// Get 从 Pool 中选择一个任意的对象，将其移出 Pool, 并返回给调用方。
// Get 可能会返回一个非零值对象（被其他人使用过），因此调用方不应假设
// 返回的对象具有任何形式的状态。
func (p *Pool) Get() any {
    if race.Enabled {
        race.Disable()
    }
    // 获取一个poolLocal
    l, pid := p.pin()
    // 先从private获取上次访问放入的对象
    x := l.private
    l.private = nil
    if x == nil {
        // Try to pop the head of the local shard. We prefer
        // the head over the tail for temporal locality of
        // reuse.
        // 尝试从shared队列获取对象
        x, _ = l.shared.popHead()
        if x == nil {
            // shared中也没有，则从其他poolLocal或者全局池中获取
            x = p.getSlow(pid)
        }
    }
    runtime_procUnpin()
    if race.Enabled {
        race.Enable()
        if x != nil {
            race.Acquire(poolRaceAddr(x))
        }
    }
    if x == nil && p.New != nil {
        // 池中没有则直接创建
        x = p.New()
    }
    return x
}

func (p *Pool) getSlow(pid int) any {
    // See the comment in pin regarding ordering of the loads.
    size := runtime_LoadAcquintptr(&p.localSize) // load-acquire
    locals := p.local                            // load-consume
    // Try to steal one element from other procs.
    for i := 0; i < int(size); i++ {
        // 获取目标 poolLocal, 引入 pid 保证不是自身
        l := indexLocal(locals, (pid+i+1)%int(size))
        // 从其他的 P 中固定的 localPool 的 share 队列的队尾偷一个缓存对象
        if x, _ := l.shared.popTail(); x != nil {
            return x
        }
    }

    // Try the victim cache. We do this after attempting to steal
    // from all primary caches because we want objects in the
    // victim cache to age out if at all possible.
    // 当 local 失败后，尝试再尝试从上一个垃圾回收周期遗留下来的 victim。
    // 如果 pid 比 victim 遗留的 localPool 还大，则说明从根据此 pid 从
    // victim 获取 localPool 会发生越界（同时也表明此时 P 的数量已经发生变化）
    // 这时无法继续读取，直接返回 nil
    size = atomic.LoadUintptr(&p.victimSize)
    if uintptr(pid) >= size {
        return nil
    }
    // 获取 localPool，并优先读取 private
    locals = p.victim
    l := indexLocal(locals, pid)
    if x := l.private; x != nil {
        l.private = nil
        return x
    }
    for i := 0; i < int(size); i++ {
        // 从其他的 P 中固定的 localPool 的 share 队列的队尾偷一个缓存对象
        l := indexLocal(locals, (pid+i)%int(size))
        if x, _ := l.shared.popTail(); x != nil {
            return x
        }
    }

    // Mark the victim cache as empty for future gets don't bother
    // with it.
    // 将 victim 缓存置空，从而确保之后的 get 操作不再读取此处的值
    atomic.StoreUintptr(&p.victimSize, 0)

    return nil
}

// pin pins the current goroutine to P, disables preemption and
// returns poolLocal pool for the P and the P's id.
// Caller must call runtime_procUnpin() when done with the pool.
// 将当前的 goroutine 固定在特定的 P（处理器）上，并返回与该 P 关联的 poolLocal 对象和 P 的 ID。
func (p *Pool) pin() (*poolLocal, int) {
    // 绑定当前P，保证后续的操作都在同一个P上执行
    pid := runtime_procPin()
    // In pinSlow we store to local and then to localSize, here we load in opposite order.
    // Since we've disabled preemption, GC cannot happen in between.
    // Thus here we must observe local at least as large localSize.
    // We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
    // 读取localSize，再读取local
    // 读取的顺序很重要，先读取localSize，再读取local，local至少是被正确初始化的
    s := runtime_LoadAcquintptr(&p.localSize) // load-acquire 确保读取的值在读取之前所有的写操作都已经完成了
    l := p.local                              // load-consume
    if uintptr(pid) < s {
        // 如果pid在local数组中，直接找到并使用
        return indexLocal(l, pid), pid
    }
    // 尝试扩展local数组
    return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {
    // Retry under the mutex.
    // Can not lock the mutex while pinned.
    // 获取全局锁，需要解除绑定状态
    runtime_procUnpin()
    allPoolsMu.Lock()
    defer allPoolsMu.Unlock()
    // 重新固定
    pid := runtime_procPin()
    // poolCleanup won't be called while we are pinned.
    // 检查local是否已经重新分配
    s := p.localSize
    l := p.local
    if uintptr(pid) < s {
        return indexLocal(l, pid), pid
    }
    // 将其添加到 allPools，垃圾回收器从这里获取所有 Pool 实例
    if p.local == nil {
        allPools = append(allPools, p)
    }
    // If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
    // 根据 P 数量创建 slice，如果 GOMAXPROCS 在 GC 间发生变化
    // 我们重新分配此数组并丢弃旧的
    size := runtime.GOMAXPROCS(0)
    local := make([]poolLocal, size)
    // 将底层数组起始指针保存到 p.local，并设置 p.localSize
    atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
    runtime_StoreReluintptr(&p.localSize, uintptr(size))     // store-release
    return &local[pid], pid
}

func poolCleanup() {
    // This function is called with the world stopped, at the beginning of a garbage collection.
    // It must not allocate and probably should not call any runtime functions.

    // Because the world is stopped, no pool user can be in a
    // pinned section (in effect, this has all Ps pinned).
    // 程序此时已经暂停，无需加锁

    // Drop victim caches from all pools.
    // 从所有的oldpools中删除victim
    for _, p := range oldPools {
        p.victim = nil
        p.victimSize = 0
    }

    // Move primary cache to victim cache.
    // 将主缓存移动到 victim 缓存
    for _, p := range allPools {
        p.victim = p.local
        p.victimSize = p.localSize
        p.local = nil
        p.localSize = 0
    }

    // The pools with non-empty primary caches now have non-empty
    // victim caches and no pools have primary caches.
    // 具有非空主缓存的池现在具有非空的 victim 缓存，并且没有任何 pool 具有主缓存。
    oldPools, allPools = allPools, nil
}

var (
    allPoolsMu Mutex

    // allPools is the set of pools that have non-empty primary
    // caches. Protected by either 1) allPoolsMu and pinning or 2)
    // STW.
    allPools []*Pool

    // oldPools is the set of pools that may have non-empty victim
    // caches. Protected by STW.
    oldPools []*Pool
)

func init() {
    // pool的垃圾回收发生在运行时GC开始之前
    // 将缓存清理函数注册到运行时 GC 时间段
    runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {
    lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
    return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())
func runtime_procPin() int
func runtime_procUnpin()

// The below are implemented in runtime/internal/atomic and the
// compiler also knows to intrinsify the symbol we linkname into this
// package.

//go:linkname runtime_LoadAcquintptr runtime/internal/atomic.LoadAcquintptr
func runtime_LoadAcquintptr(ptr *uintptr) uintptr

//go:linkname runtime_StoreReluintptr runtime/internal/atomic.StoreReluintptr
func runtime_StoreReluintptr(ptr *uintptr, val uintptr) uintptr
