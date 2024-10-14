// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
    "internal/abi"
    "internal/cpu"
    "internal/goarch"
    "runtime/internal/atomic"
    "runtime/internal/sys"
    "unsafe"
)

// set using cmd/go/internal/modload.ModInfoProg
var modinfo string

// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over worker threads.
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however it can be
//     blocked or in a syscall w/o an associated P.
//
// Design doc at https://golang.org/s/go11sched.

// Worker thread parking/unparking.
// We need to balance between keeping enough running worker threads to utilize
// available hardware parallelism and parking excessive running worker threads
// to conserve CPU resources and power. This is not simple for two reasons:
// (1) scheduler state is intentionally distributed (in particular, per-P work
// queues), so it is not possible to compute global predicates on fast paths;
// (2) for optimal thread management we would need to know the future (don't park
// a worker thread when a new goroutine will be readied in near future).
//
// Three rejected approaches that would work badly:
// 1. Centralize all scheduler state (would inhibit scalability).
// 2. Direct goroutine handoff. That is, when we ready a new goroutine and there
//    is a spare P, unpark a thread and handoff it the thread and the goroutine.
//    This would lead to thread state thrashing, as the thread that readied the
//    goroutine can be out of work the very next moment, we will need to park it.
//    Also, it would destroy locality of computation as we want to preserve
//    dependent goroutines on the same thread; and introduce additional latency.
// 3. Unpark an additional thread whenever we ready a goroutine and there is an
//    idle P, but don't do handoff. This would lead to excessive thread parking/
//    unparking as the additional threads will instantly park without discovering
//    any work to do.
//
// The current approach:
//
// This approach applies to three primary sources of potential work: readying a
// goroutine, new/modified-earlier timers, and idle-priority GC. See below for
// additional details.
//
// We unpark an additional thread when we submit work if (this is wakep()):
// 1. There is an idle P, and
// 2. There are no "spinning" worker threads.
//
// A worker thread is considered spinning if it is out of local work and did
// not find work in the global run queue or netpoller; the spinning state is
// denoted in m.spinning and in sched.nmspinning. Threads unparked this way are
// also considered spinning; we don't do goroutine handoff so such threads are
// out of work initially. Spinning threads spin on looking for work in per-P
// run queues and timer heaps or from the GC before parking. If a spinning
// thread finds work it takes itself out of the spinning state and proceeds to
// execution. If it does not find work it takes itself out of the spinning
// state and then parks.
//
// If there is at least one spinning thread (sched.nmspinning>1), we don't
// unpark new threads when submitting work. To compensate for that, if the last
// spinning thread finds work and stops spinning, it must unpark a new spinning
// thread. This approach smooths out unjustified spikes of thread unparking,
// but at the same time guarantees eventual maximal CPU parallelism
// utilization.
//
// The main implementation complication is that we need to be very careful
// during spinning->non-spinning thread transition. This transition can race
// with submission of new work, and either one part or another needs to unpark
// another worker thread. If they both fail to do that, we can end up with
// semi-persistent CPU underutilization.
//
// The general pattern for submission is:
// 1. Submit work to the local run queue, timer heap, or GC state.
// 2. #StoreLoad-style memory barrier.
// 3. Check sched.nmspinning.
//
// The general pattern for spinning->non-spinning transition is:
// 1. Decrement nmspinning.
// 2. #StoreLoad-style memory barrier.
// 3. Check all per-P work queues and GC for new work.
//
// Note that all this complexity does not apply to global run queue as we are
// not sloppy about thread unparking when submitting to global queue. Also see
// comments for nmspinning manipulation.
//
// How these different sources of work behave varies, though it doesn't affect
// the synchronization approach:
// * Ready goroutine: this is an obvious source of work; the goroutine is
//   immediately ready and must run on some thread eventually.
// * New/modified-earlier timer: The current timer implementation (see time.go)
//   uses netpoll in a thread with no work available to wait for the soonest
//   timer. If there is no thread waiting, we want a new spinning thread to go
//   wait.
// * Idle-priority GC: The GC wakes a stopped idle thread to contribute to
//   background GC work (note: currently disabled per golang.org/issue/19112).
//   Also see golang.org/issue/44313, as this should be extended to all GC
//   workers.

var (
    m0           m
    g0           g
    mcache0      *mcache
    raceprocctx0 uintptr
)

//go:linkname runtime_inittask runtime..inittask
var runtime_inittask initTask

//go:linkname main_inittask main..inittask
var main_inittask initTask

// main_init_done is a signal used by cgocallbackg that initialization
// has been completed. It is made before _cgo_notify_runtime_init_done,
// so all cgo calls can rely on it existing. When main_init is complete,
// it is closed, meaning cgocallbackg can reliably receive from it.
var main_init_done chan bool

// 用户的main函数
//go:linkname main_main main.main
func main_main()

// mainStarted indicates that the main M has started.
var mainStarted bool

// runtimeInitTime is the nanotime() at which the runtime started.
var runtimeInitTime int64

// Value to use for signal mask for newly created M's.
var initSigmask sigset

// The main goroutine.
// runtime.main函数原型
func main() {
    mp := getg().m

    // Racectx of m0->g0 is used only as the parent of the main goroutine.
    // It must not be used for anything else.
    mp.g0.racectx = 0

    // Max stack size is 1 GB on 64-bit, 250 MB on 32-bit.
    // Using decimal instead of binary GB and MB because
    // they look nicer in the stack overflow failure message.
    // 执行栈最大限制
    if goarch.PtrSize == 8 {
        maxstacksize = 1000000000
    } else {
        maxstacksize = 250000000
    }

    // An upper limit for max stack size. Used to avoid random crashes
    // after calling SetMaxStack and trying to allocate a stack that is too big,
    // since stackalloc works with 32-bit sizes.
    maxstackceiling = 2 * maxstacksize

    // Allow newproc to start new Ms.
    mainStarted = true

    if GOARCH != "wasm" { // no threads on wasm yet, so no sysmon
        // 启动系统后台监控
        systemstack(func() {
            newm(sysmon, nil, -1)
        })
    }

    // Lock the main goroutine onto this, the main OS thread,
    // during initialization. Most programs won't care, but a few
    // do require certain calls to be made by the main thread.
    // Those can arrange for main.main to run in the main thread
    // by calling runtime.LockOSThread during initialization
    // to preserve the lock.
    lockOSThread()

    if mp != &m0 {
        throw("runtime.main not on m0")
    }

    // Record when the world started.
    // Must be before doInit for tracing init.
    runtimeInitTime = nanotime()
    if runtimeInitTime == 0 {
        throw("nanotime returning zero")
    }

    if debug.inittrace != 0 {
        inittrace.id = getg().goid
        inittrace.active = true
    }

    // runtime包中的init函数执行
    doInit(&runtime_inittask) // Must be before defer.

    // Defer unlock so that runtime.Goexit during init does the unlock too.
    needUnlock := true
    defer func() {
        if needUnlock {
            unlockOSThread()
        }
    }()

    // 启动垃圾回收器后台操作
    gcenable()

    main_init_done = make(chan bool)
    if iscgo {
        if _cgo_thread_start == nil {
            throw("_cgo_thread_start missing")
        }
        if GOOS != "windows" {
            if _cgo_setenv == nil {
                throw("_cgo_setenv missing")
            }
            if _cgo_unsetenv == nil {
                throw("_cgo_unsetenv missing")
            }
        }
        if _cgo_notify_runtime_init_done == nil {
            throw("_cgo_notify_runtime_init_done missing")
        }
        // Start the template thread in case we enter Go from
        // a C-created thread and need to create a new thread.
        startTemplateThread()
        cgocall(_cgo_notify_runtime_init_done, nil)
    }

    // 执行main包中的init函数
    doInit(&main_inittask)

    // Disable init tracing after main init done to avoid overhead
    // of collecting statistics in malloc and newproc
    inittrace.active = false

    close(main_init_done)

    needUnlock = false
    unlockOSThread()

    if isarchive || islibrary {
        // A program compiled with -buildmode=c-archive or c-shared
        // has a main, but it is not executed.
        return
    }
    // 执行main函数
    fn := main_main // make an indirect call, as the linker doesn't know the address of the main package when laying down the runtime
    fn()
    if raceenabled {
        runExitHooks(0) // run hooks now, since racefini does not return
        racefini()
    }

    // Make racy client program work: if panicking on
    // another goroutine at the same time as main returns,
    // let the other goroutine finish printing the panic trace.
    // Once it does, it will exit. See issues 3934 and 20018.
    if runningPanicDefers.Load() != 0 {
        // Running deferred functions should not take long.
        for c := 0; c < 1000; c++ {
            if runningPanicDefers.Load() == 0 {
                break
            }
            Gosched()
        }
    }
    if panicking.Load() != 0 {
        gopark(nil, nil, waitReasonPanicWait, traceEvGoStop, 1)
    }
    runExitHooks(0)

    // 退出
    exit(0)
    for {
        var x *int32
        *x = 0
    }
}

// os_beforeExit is called from os.Exit(0).
//
//go:linkname os_beforeExit os.runtime_beforeExit
func os_beforeExit(exitCode int) {
    runExitHooks(exitCode)
    if exitCode == 0 && raceenabled {
        racefini()
    }
}

// start forcegc helper goroutine
func init() {
    go forcegchelper()
}

func forcegchelper() {
    forcegc.g = getg()
    lockInit(&forcegc.lock, lockRankForcegc)
    for {
        lock(&forcegc.lock)
        if forcegc.idle.Load() {
            throw("forcegc: phase error")
        }
        forcegc.idle.Store(true)
        goparkunlock(&forcegc.lock, waitReasonForceGCIdle, traceEvGoBlock, 1)
        // this goroutine is explicitly resumed by sysmon
        if debug.gctrace > 0 {
            println("GC forced")
        }
        // Time-triggered, fully concurrent.
        gcStart(gcTrigger{kind: gcTriggerTime, now: nanotime()})
    }
}

//go:nosplit

// Gosched yields the processor, allowing other goroutines to run. It does not
// suspend the current goroutine, so execution resumes automatically.
func Gosched() {
    checkTimeouts()
    mcall(gosched_m)
}

// goschedguarded yields the processor like gosched, but also checks
// for forbidden states and opts out of the yield in those cases.
//
//go:nosplit
func goschedguarded() {
    mcall(goschedguarded_m)
}

// goschedIfBusy yields the processor like gosched, but only does so if
// there are no idle Ps or if we're on the only P and there's nothing in
// the run queue. In both cases, there is freely available idle time.
//
//go:nosplit
func goschedIfBusy() {
    gp := getg()
    // Call gosched if gp.preempt is set; we may be in a tight loop that
    // doesn't otherwise yield.
    if !gp.preempt && sched.npidle.Load() > 0 {
        return
    }
    mcall(gosched_m)
}

// Puts the current goroutine into a waiting state and calls unlockf on the
// system stack.
//
// If unlockf returns false, the goroutine is resumed.
//
// unlockf must not access this G's stack, as it may be moved between
// the call to gopark and the call to unlockf.
//
// Note that because unlockf is called after putting the G into a waiting
// state, the G may have already been readied by the time unlockf is called
// unless there is external synchronization preventing the G from being
// readied. If unlockf returns false, it must guarantee that the G cannot be
// externally readied.
//
// Reason explains why the goroutine has been parked. It is displayed in stack
// traces and heap dumps. Reasons should be unique and descriptive. Do not
// re-use reasons, add new ones.
func gopark(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer, reason waitReason, traceEv byte, traceskip int) {
    if reason != waitReasonSleep {
        checkTimeouts() // timeouts may expire while two goroutines keep the scheduler busy
    }
    mp := acquirem()
    gp := mp.curg
    status := readgstatus(gp)
    if status != _Grunning && status != _Gscanrunning {
        throw("gopark: bad g status")
    }
    mp.waitlock = lock
    mp.waitunlockf = unlockf
    gp.waitreason = reason
    mp.waittraceev = traceEv
    mp.waittraceskip = traceskip
    releasem(mp)
    // can't do anything that might move the G between Ms here.
    mcall(park_m)
}

// Puts the current goroutine into a waiting state and unlocks the lock.
// The goroutine can be made runnable again by calling goready(gp).
func goparkunlock(lock *mutex, reason waitReason, traceEv byte, traceskip int) {
    gopark(parkunlock_c, unsafe.Pointer(lock), reason, traceEv, traceskip)
}

func goready(gp *g, traceskip int) {
    systemstack(func() {
        ready(gp, traceskip, true)
    })
}

//go:nosplit
func acquireSudog() *sudog {
    // Delicate dance: the semaphore implementation calls
    // acquireSudog, acquireSudog calls new(sudog),
    // new calls malloc, malloc can call the garbage collector,
    // and the garbage collector calls the semaphore implementation
    // in stopTheWorld.
    // Break the cycle by doing acquirem/releasem around new(sudog).
    // The acquirem/releasem increments m.locks during new(sudog),
    // which keeps the garbage collector from being invoked.
    mp := acquirem()
    pp := mp.p.ptr()
    if len(pp.sudogcache) == 0 {
        lock(&sched.sudoglock)
        // First, try to grab a batch from central cache.
        for len(pp.sudogcache) < cap(pp.sudogcache)/2 && sched.sudogcache != nil {
            s := sched.sudogcache
            sched.sudogcache = s.next
            s.next = nil
            pp.sudogcache = append(pp.sudogcache, s)
        }
        unlock(&sched.sudoglock)
        // If the central cache is empty, allocate a new one.
        if len(pp.sudogcache) == 0 {
            pp.sudogcache = append(pp.sudogcache, new(sudog))
        }
    }
    n := len(pp.sudogcache)
    s := pp.sudogcache[n-1]
    pp.sudogcache[n-1] = nil
    pp.sudogcache = pp.sudogcache[:n-1]
    if s.elem != nil {
        throw("acquireSudog: found s.elem != nil in cache")
    }
    releasem(mp)
    return s
}

//go:nosplit
func releaseSudog(s *sudog) {
    if s.elem != nil {
        throw("runtime: sudog with non-nil elem")
    }
    if s.isSelect {
        throw("runtime: sudog with non-false isSelect")
    }
    if s.next != nil {
        throw("runtime: sudog with non-nil next")
    }
    if s.prev != nil {
        throw("runtime: sudog with non-nil prev")
    }
    if s.waitlink != nil {
        throw("runtime: sudog with non-nil waitlink")
    }
    if s.c != nil {
        throw("runtime: sudog with non-nil c")
    }
    gp := getg()
    if gp.param != nil {
        throw("runtime: releaseSudog with non-nil gp.param")
    }
    mp := acquirem() // avoid rescheduling to another P
    pp := mp.p.ptr()
    if len(pp.sudogcache) == cap(pp.sudogcache) {
        // Transfer half of local cache to the central cache.
        var first, last *sudog
        for len(pp.sudogcache) > cap(pp.sudogcache)/2 {
            n := len(pp.sudogcache)
            p := pp.sudogcache[n-1]
            pp.sudogcache[n-1] = nil
            pp.sudogcache = pp.sudogcache[:n-1]
            if first == nil {
                first = p
            } else {
                last.next = p
            }
            last = p
        }
        lock(&sched.sudoglock)
        last.next = sched.sudogcache
        sched.sudogcache = first
        unlock(&sched.sudoglock)
    }
    pp.sudogcache = append(pp.sudogcache, s)
    releasem(mp)
}

// called from assembly.
func badmcall(fn func(*g)) {
    throw("runtime: mcall called on m->g0 stack")
}

func badmcall2(fn func(*g)) {
    throw("runtime: mcall function returned")
}

func badreflectcall() {
    panic(plainError("arg size to reflect.call more than 1GB"))
}

//go:nosplit
//go:nowritebarrierrec
func badmorestackg0() {
    writeErrStr("fatal: morestack on g0\n")
}

//go:nosplit
//go:nowritebarrierrec
func badmorestackgsignal() {
    writeErrStr("fatal: morestack on gsignal\n")
}

//go:nosplit
func badctxt() {
    throw("ctxt != 0")
}

func lockedOSThread() bool {
    gp := getg()
    return gp.lockedm != 0 && gp.m.lockedg != 0
}

var (
    // allgs contains all Gs ever created (including dead Gs), and thus
    // never shrinks.
    //
    // Access via the slice is protected by allglock or stop-the-world.
    // Readers that cannot take the lock may (carefully!) use the atomic
    // variables below.
    allglock mutex
    allgs    []*g

    // allglen and allgptr are atomic variables that contain len(allgs) and
    // &allgs[0] respectively. Proper ordering depends on totally-ordered
    // loads and stores. Writes are protected by allglock.
    //
    // allgptr is updated before allglen. Readers should read allglen
    // before allgptr to ensure that allglen is always <= len(allgptr). New
    // Gs appended during the race can be missed. For a consistent view of
    // all Gs, allglock must be held.
    //
    // allgptr copies should always be stored as a concrete type or
    // unsafe.Pointer, not uintptr, to ensure that GC can still reach it
    // even if it points to a stale array.
    allglen uintptr
    allgptr **g
)

func allgadd(gp *g) {
    if readgstatus(gp) == _Gidle {
        throw("allgadd: bad status Gidle")
    }

    lock(&allglock)
    allgs = append(allgs, gp)
    if &allgs[0] != allgptr {
        atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&allgs[0]))
    }
    atomic.Storeuintptr(&allglen, uintptr(len(allgs)))
    unlock(&allglock)
}

// allGsSnapshot returns a snapshot of the slice of all Gs.
//
// The world must be stopped or allglock must be held.
func allGsSnapshot() []*g {
    assertWorldStoppedOrLockHeld(&allglock)

    // Because the world is stopped or allglock is held, allgadd
    // cannot happen concurrently with this. allgs grows
    // monotonically and existing entries never change, so we can
    // simply return a copy of the slice header. For added safety,
    // we trim everything past len because that can still change.
    return allgs[:len(allgs):len(allgs)]
}

// atomicAllG returns &allgs[0] and len(allgs) for use with atomicAllGIndex.
func atomicAllG() (**g, uintptr) {
    length := atomic.Loaduintptr(&allglen)
    ptr := (**g)(atomic.Loadp(unsafe.Pointer(&allgptr)))
    return ptr, length
}

// atomicAllGIndex returns ptr[i] with the allgptr returned from atomicAllG.
func atomicAllGIndex(ptr **g, i uintptr) *g {
    return *(**g)(add(unsafe.Pointer(ptr), i*goarch.PtrSize))
}

// forEachG calls fn on every G from allgs.
//
// forEachG takes a lock to exclude concurrent addition of new Gs.
func forEachG(fn func(gp *g)) {
    lock(&allglock)
    for _, gp := range allgs {
        fn(gp)
    }
    unlock(&allglock)
}

// forEachGRace calls fn on every G from allgs.
//
// forEachGRace avoids locking, but does not exclude addition of new Gs during
// execution, which may be missed.
func forEachGRace(fn func(gp *g)) {
    ptr, length := atomicAllG()
    for i := uintptr(0); i < length; i++ {
        gp := atomicAllGIndex(ptr, i)
        fn(gp)
    }
    return
}

const (
    // Number of goroutine ids to grab from sched.goidgen to local per-P cache at once.
    // 16 seems to provide enough amortization, but other than that it's mostly arbitrary number.
    _GoidCacheBatch = 16
)

// cpuinit sets up CPU feature flags and calls internal/cpu.Initialize. env should be the complete
// value of the GODEBUG environment variable.
func cpuinit(env string) {
    switch GOOS {
    case "aix", "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd", "illumos", "solaris", "linux":
        cpu.DebugOptions = true
    }
    cpu.Initialize(env)

    // Support cpu feature variables are used in code generated by the compiler
    // to guard execution of instructions that can not be assumed to be always supported.
    // 支持 CPU 特性的变量由编译器生成的代码来阻止指令的执行，从而不能假设总是支持的
    switch GOARCH {
    case "386", "amd64":
        x86HasPOPCNT = cpu.X86.HasPOPCNT
        x86HasSSE41 = cpu.X86.HasSSE41
        x86HasFMA = cpu.X86.HasFMA

    case "arm":
        armHasVFPv4 = cpu.ARM.HasVFPv4

    case "arm64":
        arm64HasATOMICS = cpu.ARM64.HasATOMICS
    }
}

// getGodebugEarly extracts the environment variable GODEBUG from the environment on
// Unix-like operating systems and returns it. This function exists to extract GODEBUG
// early before much of the runtime is initialized.
func getGodebugEarly() string {
    const prefix = "GODEBUG="
    var env string
    switch GOOS {
    case "aix", "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd", "illumos", "solaris", "linux":
        // Similar to goenv_unix but extracts the environment value for
        // GODEBUG directly.
        // TODO(moehrmann): remove when general goenvs() can be called before cpuinit()
        n := int32(0)
        for argv_index(argv, argc+1+n) != nil {
            n++
        }

        for i := int32(0); i < n; i++ {
            p := argv_index(argv, argc+1+i)
            s := unsafe.String(p, findnull(p))

            if hasPrefix(s, prefix) {
                env = gostring(p)[len(prefix):]
                break
            }
        }
    }
    return env
}

// The bootstrap sequence is:
//
//	call osinit
//	call schedinit
//	make & queue new G
//	call runtime·mstart
//
// The new G calls runtime·main.
// todo 待研究
func schedinit() {
    lockInit(&sched.lock, lockRankSched)
    lockInit(&sched.sysmonlock, lockRankSysmon)
    lockInit(&sched.deferlock, lockRankDefer)
    lockInit(&sched.sudoglock, lockRankSudog)
    lockInit(&deadlock, lockRankDeadlock)
    lockInit(&paniclk, lockRankPanic)
    lockInit(&allglock, lockRankAllg)
    lockInit(&allpLock, lockRankAllp)
    lockInit(&reflectOffs.lock, lockRankReflectOffs)
    lockInit(&finlock, lockRankFin)
    lockInit(&trace.bufLock, lockRankTraceBuf)
    lockInit(&trace.stringsLock, lockRankTraceStrings)
    lockInit(&trace.lock, lockRankTrace)
    lockInit(&cpuprof.lock, lockRankCpuprof)
    lockInit(&trace.stackTab.lock, lockRankTraceStackTab)
    allocmLock.init(lockRankAllocmR, lockRankAllocmRInternal, lockRankAllocmW)
    execLock.init(lockRankExecR, lockRankExecRInternal, lockRankExecW)
    // Enforce that this lock is always a leaf lock.
    // All of this lock's critical sections should be
    // extremely short.
    lockInit(&memstats.heapStats.noPLock, lockRankLeafRank)

    // raceinit must be the first call to race detector.
    // In particular, it must be done before mallocinit below calls racemapshadow.
    gp := getg()
    if raceenabled {
        gp.racectx, raceprocctx0 = raceinit()
    }

    // 最大系统线程数量限制
    sched.maxmcount = 10000

    // The world starts stopped.
    // go运行时是否处于停止状态
    worldStopped()

    moduledataverify()
    stackinit()  // 初始化执行栈
    mallocinit() // 初始化内存分配器
    godebug := getGodebugEarly()
    initPageTrace(godebug) // must run after mallocinit but before anything allocates
    cpuinit(godebug)       // must run before alginit
    alginit()              // maps, hash, fastrand must not be used before this call
    fastrandinit()         // must run before mcommoninit
    mcommoninit(gp.m, -1)  // 初始化当前系统线程
    modulesinit()          // provides activeModules
    typelinksinit()        // uses maps, activeModules
    // 接口相关初始化
    itabsinit()  // uses activeModules
    stkobjinit() // must run before GC starts

    sigsave(&gp.m.sigmask)
    initSigmask = gp.m.sigmask

    goargs()
    goenvs()
    secure()
    parsedebugvars()
    gcinit() // 垃圾回收器初始化

    // if disableMemoryProfiling is set, update MemProfileRate to 0 to turn off memprofile.
    // Note: parsedebugvars may update MemProfileRate, but when disableMemoryProfiling is
    // set to true by the linker, it means that nothing is consuming the profile, it is
    // safe to set MemProfileRate to 0.
    if disableMemoryProfiling {
        MemProfileRate = 0
    }

    lock(&sched.lock)
    sched.lastpoll.Store(nanotime())
    // 创建P
    // 确定P的数量
    procs := ncpu
    if n, ok := atoi32(gogetenv("GOMAXPROCS")); ok && n > 0 {
        procs = n
    }
    if procresize(procs) != nil {
        throw("unknown runnable goroutine during bootstrap")
    }
    unlock(&sched.lock)

    // World is effectively started now, as P's can run.
    worldStarted()

    // For cgocheck > 1, we turn on the write barrier at all times
    // and check all pointer writes. We can't do this until after
    // procresize because the write barrier needs a P.
    if debug.cgocheck > 1 {
        writeBarrier.cgo = true
        writeBarrier.enabled = true
        for _, pp := range allp {
            pp.wbBuf.reset()
        }
    }

    if buildVersion == "" {
        // Condition should never trigger. This code just serves
        // to ensure runtime·buildVersion is kept in the resulting binary.
        buildVersion = "unknown"
    }
    if len(modinfo) == 1 {
        // Condition should never trigger. This code just serves
        // to ensure runtime·modinfo is kept in the resulting binary.
        modinfo = ""
    }
}

func dumpgstatus(gp *g) {
    thisg := getg()
    print("runtime:   gp: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
    print("runtime: getg:  g=", thisg, ", goid=", thisg.goid, ",  g->atomicstatus=", readgstatus(thisg), "\n")
}

// sched.lock must be held.
func checkmcount() {
    assertLockHeld(&sched.lock)

    if mcount() > sched.maxmcount {
        print("runtime: program exceeds ", sched.maxmcount, "-thread limit\n")
        throw("thread exhaustion")
    }
}

// mReserveID returns the next ID to use for a new m. This new m is immediately
// considered 'running' by checkdead.
// mReserveID 返回用于新 m 的下一个 ID。这个新的 m 立即被 checkdead 视为“正在运行”。
//
// sched.lock must be held.
func mReserveID() int64 {
    assertLockHeld(&sched.lock)

    if sched.mnext+1 < sched.mnext {
        throw("runtime: thread ID overflow")
    }
    // mnext 表示当前 m 的数量，还表示下一个 m 的 id
    id := sched.mnext
    // 增加 m 的数量
    sched.mnext++
    checkmcount()
    return id
}

// Pre-allocated ID may be passed as 'id', or omitted by passing -1.
func mcommoninit(mp *m, id int64) {
    gp := getg()

    // g0 stack won't make sense for user (and is not necessary unwindable).
    if gp != gp.m.g0 {
        callers(1, mp.createstack[:])
    }

    lock(&sched.lock)

    if id >= 0 {
        mp.id = id
    } else {
        mp.id = mReserveID()
    }

    lo := uint32(int64Hash(uint64(mp.id), fastrandseed))
    hi := uint32(int64Hash(uint64(cputicks()), ^fastrandseed))
    if lo|hi == 0 {
        hi = 1
    }
    // Same behavior as for 1.17.
    // TODO: Simplify ths.
    if goarch.BigEndian {
        mp.fastrand = uint64(lo)<<32 | uint64(hi)
    } else {
        mp.fastrand = uint64(hi)<<32 | uint64(lo)
    }

    // 初始化gsignal，用于处理m上的信号
    mpreinit(mp)
    if mp.gsignal != nil {
        mp.gsignal.stackguard1 = mp.gsignal.stack.lo + _StackGuard
    }

    // Add to allm so garbage collector doesn't free g->m
    // when it is just in a register or thread-local storage.
    // 添加到 allm 中，从而当它刚保存到寄存器或本地线程存储时候 GC 不会释放 g.m
    mp.alllink = allm

    // NumCgoCall() iterates over allm w/o schedlock,
    // so we need to publish it safely.
    // NumCgoCall() 会在没有使用 schedlock 时遍历 allm，等价于 allm = mp
    atomicstorep(unsafe.Pointer(&allm), unsafe.Pointer(mp))
    unlock(&sched.lock)

    // Allocate memory to hold a cgo traceback if the cgo call crashes.
    if iscgo || GOOS == "solaris" || GOOS == "illumos" || GOOS == "windows" {
        mp.cgoCallers = new(cgoCallers)
    }
}

func (mp *m) becomeSpinning() {
    mp.spinning = true
    sched.nmspinning.Add(1)
    sched.needspinning.Store(0)
}

var fastrandseed uintptr

func fastrandinit() {
    s := (*[unsafe.Sizeof(fastrandseed)]byte)(unsafe.Pointer(&fastrandseed))[:]
    getRandomData(s)
}

// Mark gp ready to run.
func ready(gp *g, traceskip int, next bool) {
    if trace.enabled {
        traceGoUnpark(gp, traceskip)
    }

    status := readgstatus(gp)

    // Mark runnable.
    mp := acquirem() // disable preemption because it can be holding p in a local var
    if status&^_Gscan != _Gwaiting {
        dumpgstatus(gp)
        throw("bad g->status in ready")
    }

    // status is Gwaiting or Gscanwaiting, make Grunnable and put on runq
    casgstatus(gp, _Gwaiting, _Grunnable)
    runqput(mp.p.ptr(), gp, next)
    wakep()
    releasem(mp)
}

// freezeStopWait is a large value that freezetheworld sets
// sched.stopwait to in order to request that all Gs permanently stop.
const freezeStopWait = 0x7fffffff

// freezing is set to non-zero if the runtime is trying to freeze the
// world.
var freezing atomic.Bool

// Similar to stopTheWorld but best-effort and can be called several times.
// There is no reverse operation, used during crashing.
// This function must not lock any mutexes.
func freezetheworld() {
    freezing.Store(true)
    // stopwait and preemption requests can be lost
    // due to races with concurrently executing threads,
    // so try several times
    for i := 0; i < 5; i++ {
        // this should tell the scheduler to not start any new goroutines
        sched.stopwait = freezeStopWait
        sched.gcwaiting.Store(true)
        // this should stop running goroutines
        if !preemptall() {
            break // no running goroutines
        }
        usleep(1000)
    }
    // to be sure
    usleep(1000)
    preemptall()
    usleep(1000)
}

// All reads and writes of g's status go through readgstatus, casgstatus
// castogscanstatus, casfrom_Gscanstatus.
//
//go:nosplit
func readgstatus(gp *g) uint32 {
    return gp.atomicstatus.Load()
}

// The Gscanstatuses are acting like locks and this releases them.
// If it proves to be a performance hit we should be able to make these
// simple atomic stores but for now we are going to throw if
// we see an inconsistent state.
func casfrom_Gscanstatus(gp *g, oldval, newval uint32) {
    success := false

    // Check that transition is valid.
    switch oldval {
    default:
        print("runtime: casfrom_Gscanstatus bad oldval gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
        dumpgstatus(gp)
        throw("casfrom_Gscanstatus:top gp->status is not in scan state")
    case _Gscanrunnable,
        _Gscanwaiting,
        _Gscanrunning,
        _Gscansyscall,
        _Gscanpreempted:
        if newval == oldval&^_Gscan {
            success = gp.atomicstatus.CompareAndSwap(oldval, newval)
        }
    }
    if !success {
        print("runtime: casfrom_Gscanstatus failed gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
        dumpgstatus(gp)
        throw("casfrom_Gscanstatus: gp->status is not in scan state")
    }
    releaseLockRank(lockRankGscan)
}

// This will return false if the gp is not in the expected status and the cas fails.
// This acts like a lock acquire while the casfromgstatus acts like a lock release.
func castogscanstatus(gp *g, oldval, newval uint32) bool {
    switch oldval {
    case _Grunnable,
        _Grunning,
        _Gwaiting,
        _Gsyscall:
        if newval == oldval|_Gscan {
            r := gp.atomicstatus.CompareAndSwap(oldval, newval)
            if r {
                acquireLockRank(lockRankGscan)
            }
            return r

        }
    }
    print("runtime: castogscanstatus oldval=", hex(oldval), " newval=", hex(newval), "\n")
    throw("castogscanstatus")
    panic("not reached")
}

// casgstatusAlwaysTrack is a debug flag that causes casgstatus to always track
// various latencies on every transition instead of sampling them.
var casgstatusAlwaysTrack = false

// If asked to move to or from a Gscanstatus this will throw. Use the castogscanstatus
// and casfrom_Gscanstatus instead.
// casgstatus will loop if the g->atomicstatus is in a Gscan status until the routine that
// put it in the Gscan state is finished.
//
//go:nosplit
func casgstatus(gp *g, oldval, newval uint32) {
    if (oldval&_Gscan != 0) || (newval&_Gscan != 0) || oldval == newval {
        systemstack(func() {
            print("runtime: casgstatus: oldval=", hex(oldval), " newval=", hex(newval), "\n")
            throw("casgstatus: bad incoming values")
        })
    }

    acquireLockRank(lockRankGscan)
    releaseLockRank(lockRankGscan)

    // See https://golang.org/cl/21503 for justification of the yield delay.
    const yieldDelay = 5 * 1000
    var nextYield int64

    // loop if gp->atomicstatus is in a scan state giving
    // GC time to finish and change the state to oldval.
    for i := 0; !gp.atomicstatus.CompareAndSwap(oldval, newval); i++ {
        if oldval == _Gwaiting && gp.atomicstatus.Load() == _Grunnable {
            throw("casgstatus: waiting for Gwaiting but is Grunnable")
        }
        if i == 0 {
            nextYield = nanotime() + yieldDelay
        }
        if nanotime() < nextYield {
            for x := 0; x < 10 && gp.atomicstatus.Load() != oldval; x++ {
                procyield(1)
            }
        } else {
            osyield()
            nextYield = nanotime() + yieldDelay/2
        }
    }

    if oldval == _Grunning {
        // Track every gTrackingPeriod time a goroutine transitions out of running.
        if casgstatusAlwaysTrack || gp.trackingSeq%gTrackingPeriod == 0 {
            gp.tracking = true
        }
        gp.trackingSeq++
    }
    if !gp.tracking {
        return
    }

    // Handle various kinds of tracking.
    //
    // Currently:
    // - Time spent in runnable.
    // - Time spent blocked on a sync.Mutex or sync.RWMutex.
    switch oldval {
    case _Grunnable:
        // We transitioned out of runnable, so measure how much
        // time we spent in this state and add it to
        // runnableTime.
        now := nanotime()
        gp.runnableTime += now - gp.trackingStamp
        gp.trackingStamp = 0
    case _Gwaiting:
        if !gp.waitreason.isMutexWait() {
            // Not blocking on a lock.
            break
        }
        // Blocking on a lock, measure it. Note that because we're
        // sampling, we have to multiply by our sampling period to get
        // a more representative estimate of the absolute value.
        // gTrackingPeriod also represents an accurate sampling period
        // because we can only enter this state from _Grunning.
        now := nanotime()
        sched.totalMutexWaitTime.Add((now - gp.trackingStamp) * gTrackingPeriod)
        gp.trackingStamp = 0
    }
    switch newval {
    case _Gwaiting:
        if !gp.waitreason.isMutexWait() {
            // Not blocking on a lock.
            break
        }
        // Blocking on a lock. Write down the timestamp.
        now := nanotime()
        gp.trackingStamp = now
    case _Grunnable:
        // We just transitioned into runnable, so record what
        // time that happened.
        now := nanotime()
        gp.trackingStamp = now
    case _Grunning:
        // We're transitioning into running, so turn off
        // tracking and record how much time we spent in
        // runnable.
        gp.tracking = false
        sched.timeToRun.record(gp.runnableTime)
        gp.runnableTime = 0
    }
}

// casGToWaiting transitions gp from old to _Gwaiting, and sets the wait reason.
//
// Use this over casgstatus when possible to ensure that a waitreason is set.
func casGToWaiting(gp *g, old uint32, reason waitReason) {
    // Set the wait reason before calling casgstatus, because casgstatus will use it.
    gp.waitreason = reason
    casgstatus(gp, old, _Gwaiting)
}

// casgstatus(gp, oldstatus, Gcopystack), assuming oldstatus is Gwaiting or Grunnable.
// Returns old status. Cannot call casgstatus directly, because we are racing with an
// async wakeup that might come in from netpoll. If we see Gwaiting from the readgstatus,
// it might have become Grunnable by the time we get to the cas. If we called casgstatus,
// it would loop waiting for the status to go back to Gwaiting, which it never will.
//
//go:nosplit
func casgcopystack(gp *g) uint32 {
    for {
        oldstatus := readgstatus(gp) &^ _Gscan
        if oldstatus != _Gwaiting && oldstatus != _Grunnable {
            throw("copystack: bad status, not Gwaiting or Grunnable")
        }
        if gp.atomicstatus.CompareAndSwap(oldstatus, _Gcopystack) {
            return oldstatus
        }
    }
}

// casGToPreemptScan transitions gp from _Grunning to _Gscan|_Gpreempted.
//
// TODO(austin): This is the only status operation that both changes
// the status and locks the _Gscan bit. Rethink this.
func casGToPreemptScan(gp *g, old, new uint32) {
    if old != _Grunning || new != _Gscan|_Gpreempted {
        throw("bad g transition")
    }
    acquireLockRank(lockRankGscan)
    for !gp.atomicstatus.CompareAndSwap(_Grunning, _Gscan|_Gpreempted) {
    }
}

// casGFromPreempted attempts to transition gp from _Gpreempted to
// _Gwaiting. If successful, the caller is responsible for
// re-scheduling gp.
func casGFromPreempted(gp *g, old, new uint32) bool {
    if old != _Gpreempted || new != _Gwaiting {
        throw("bad g transition")
    }
    gp.waitreason = waitReasonPreempted
    return gp.atomicstatus.CompareAndSwap(_Gpreempted, _Gwaiting)
}

// stopTheWorld stops all P's from executing goroutines, interrupting
// all goroutines at GC safe points and records reason as the reason
// for the stop. On return, only the current goroutine's P is running.
// stopTheWorld must not be called from a system stack and the caller
// must not hold worldsema. The caller must call startTheWorld when
// other P's should resume execution.
//
// stopTheWorld is safe for multiple goroutines to call at the
// same time. Each will execute its own stop, and the stops will
// be serialized.
//
// This is also used by routines that do stack dumps. If the system is
// in panic or being exited, this may not reliably stop all
// goroutines.
func stopTheWorld(reason string) {
    semacquire(&worldsema)
    gp := getg()
    gp.m.preemptoff = reason
    systemstack(func() {
        // Mark the goroutine which called stopTheWorld preemptible so its
        // stack may be scanned.
        // This lets a mark worker scan us while we try to stop the world
        // since otherwise we could get in a mutual preemption deadlock.
        // We must not modify anything on the G stack because a stack shrink
        // may occur. A stack shrink is otherwise OK though because in order
        // to return from this function (and to leave the system stack) we
        // must have preempted all goroutines, including any attempting
        // to scan our stack, in which case, any stack shrinking will
        // have already completed by the time we exit.
        // Don't provide a wait reason because we're still executing.
        casGToWaiting(gp, _Grunning, waitReasonStoppingTheWorld)
        stopTheWorldWithSema()
        casgstatus(gp, _Gwaiting, _Grunning)
    })
}

// startTheWorld undoes the effects of stopTheWorld.
func startTheWorld() {
    systemstack(func() { startTheWorldWithSema(false) })

    // worldsema must be held over startTheWorldWithSema to ensure
    // gomaxprocs cannot change while worldsema is held.
    //
    // Release worldsema with direct handoff to the next waiter, but
    // acquirem so that semrelease1 doesn't try to yield our time.
    //
    // Otherwise if e.g. ReadMemStats is being called in a loop,
    // it might stomp on other attempts to stop the world, such as
    // for starting or ending GC. The operation this blocks is
    // so heavy-weight that we should just try to be as fair as
    // possible here.
    //
    // We don't want to just allow us to get preempted between now
    // and releasing the semaphore because then we keep everyone
    // (including, for example, GCs) waiting longer.
    mp := acquirem()
    mp.preemptoff = ""
    semrelease1(&worldsema, true, 0)
    releasem(mp)
}

// stopTheWorldGC has the same effect as stopTheWorld, but blocks
// until the GC is not running. It also blocks a GC from starting
// until startTheWorldGC is called.
func stopTheWorldGC(reason string) {
    semacquire(&gcsema)
    stopTheWorld(reason)
}

// startTheWorldGC undoes the effects of stopTheWorldGC.
func startTheWorldGC() {
    startTheWorld()
    semrelease(&gcsema)
}

// Holding worldsema grants an M the right to try to stop the world.
var worldsema uint32 = 1

// Holding gcsema grants the M the right to block a GC, and blocks
// until the current GC is done. In particular, it prevents gomaxprocs
// from changing concurrently.
//
// TODO(mknyszek): Once gomaxprocs and the execution tracer can handle
// being changed/enabled during a GC, remove this.
var gcsema uint32 = 1

// stopTheWorldWithSema is the core implementation of stopTheWorld.
// The caller is responsible for acquiring worldsema and disabling
// preemption first and then should stopTheWorldWithSema on the system
// stack:
//
//	semacquire(&worldsema, 0)
//	m.preemptoff = "reason"
//	systemstack(stopTheWorldWithSema)
//
// When finished, the caller must either call startTheWorld or undo
// these three operations separately:
//
//	m.preemptoff = ""
//	systemstack(startTheWorldWithSema)
//	semrelease(&worldsema)
//
// It is allowed to acquire worldsema once and then execute multiple
// startTheWorldWithSema/stopTheWorldWithSema pairs.
// Other P's are able to execute between successive calls to
// startTheWorldWithSema and stopTheWorldWithSema.
// Holding worldsema causes any other goroutines invoking
// stopTheWorld to block.
func stopTheWorldWithSema() {
    gp := getg()

    // If we hold a lock, then we won't be able to stop another M
    // that is blocked trying to acquire the lock.
    if gp.m.locks > 0 {
        throw("stopTheWorld: holding locks")
    }

    lock(&sched.lock)
    sched.stopwait = gomaxprocs
    sched.gcwaiting.Store(true)
    preemptall()
    // stop current P
    gp.m.p.ptr().status = _Pgcstop // Pgcstop is only diagnostic.
    sched.stopwait--
    // try to retake all P's in Psyscall status
    for _, pp := range allp {
        s := pp.status
        if s == _Psyscall && atomic.Cas(&pp.status, s, _Pgcstop) {
            if trace.enabled {
                traceGoSysBlock(pp)
                traceProcStop(pp)
            }
            pp.syscalltick++
            sched.stopwait--
        }
    }
    // stop idle P's
    now := nanotime()
    for {
        pp, _ := pidleget(now)
        if pp == nil {
            break
        }
        pp.status = _Pgcstop
        sched.stopwait--
    }
    wait := sched.stopwait > 0
    unlock(&sched.lock)

    // wait for remaining P's to stop voluntarily
    if wait {
        for {
            // wait for 100us, then try to re-preempt in case of any races
            if notetsleep(&sched.stopnote, 100*1000) {
                noteclear(&sched.stopnote)
                break
            }
            preemptall()
        }
    }

    // sanity checks
    bad := ""
    if sched.stopwait != 0 {
        bad = "stopTheWorld: not stopped (stopwait != 0)"
    } else {
        for _, pp := range allp {
            if pp.status != _Pgcstop {
                bad = "stopTheWorld: not stopped (status != _Pgcstop)"
            }
        }
    }
    if freezing.Load() {
        // Some other thread is panicking. This can cause the
        // sanity checks above to fail if the panic happens in
        // the signal handler on a stopped thread. Either way,
        // we should halt this thread.
        lock(&deadlock)
        lock(&deadlock)
    }
    if bad != "" {
        throw(bad)
    }

    worldStopped()
}

func startTheWorldWithSema(emitTraceEvent bool) int64 {
    assertWorldStopped()

    mp := acquirem() // disable preemption because it can be holding p in a local var
    if netpollinited() {
        list := netpoll(0) // non-blocking
        injectglist(&list)
    }
    lock(&sched.lock)

    procs := gomaxprocs
    if newprocs != 0 {
        procs = newprocs
        newprocs = 0
    }
    p1 := procresize(procs)
    sched.gcwaiting.Store(false)
    if sched.sysmonwait.Load() {
        sched.sysmonwait.Store(false)
        notewakeup(&sched.sysmonnote)
    }
    unlock(&sched.lock)

    worldStarted()

    for p1 != nil {
        p := p1
        p1 = p1.link.ptr()
        if p.m != 0 {
            mp := p.m.ptr()
            p.m = 0
            if mp.nextp != 0 {
                throw("startTheWorld: inconsistent mp->nextp")
            }
            mp.nextp.set(p)
            notewakeup(&mp.park)
        } else {
            // Start M to run P.  Do not start another M below.
            newm(nil, p, -1)
        }
    }

    // Capture start-the-world time before doing clean-up tasks.
    startTime := nanotime()
    if emitTraceEvent {
        traceGCSTWDone()
    }

    // Wakeup an additional proc in case we have excessive runnable goroutines
    // in local queues or in the global queue. If we don't, the proc will park itself.
    // If we have lots of excessive work, resetspinning will unpark additional procs as necessary.
    wakep()

    releasem(mp)

    return startTime
}

// usesLibcall indicates whether this runtime performs system calls
// via libcall.
func usesLibcall() bool {
    switch GOOS {
    case "aix", "darwin", "illumos", "ios", "solaris", "windows":
        return true
    case "openbsd":
        return GOARCH == "386" || GOARCH == "amd64" || GOARCH == "arm" || GOARCH == "arm64"
    }
    return false
}

// mStackIsSystemAllocated indicates whether this runtime starts on a
// system-allocated stack.
func mStackIsSystemAllocated() bool {
    // 由于 windows, solaris, darwin, aix 和 plan9 总是系统分配的栈，在 mstart 之前放进 _g_.stack 的
    // 因此上面的逻辑还没有设置 osStack。
    switch GOOS {
    case "aix", "darwin", "plan9", "illumos", "ios", "solaris", "windows":
        return true
    case "openbsd":
        switch GOARCH {
        case "386", "amd64", "arm", "arm64":
            return true
        }
    }
    return false
}

// mstart is the entry-point for new Ms.
// It is written in assembly, uses ABI0, is marked TOPFRAME, and calls mstart0.
func mstart()

// mstart0 is the Go entry-point for new Ms.
// This must not split the stack because we may not even have stack
// bounds set up yet.
//
// May run during STW (because it doesn't have a P yet), so write
// barriers are not allowed.
//
//go:nosplit
//go:nowritebarrierrec
func mstart0() {
    gp := getg()

    // 开始确定执行栈的边界了
    // 通过检查 g 执行栈的边界来确定是否为系统栈
    osStack := gp.stack.lo == 0
    if osStack {
        // Initialize stack bounds from system stack.
        // Cgo may have left stack size in stack.hi.
        // minit may update the stack bounds.
        //
        // Note: these bounds may not be very accurate.
        // We set hi to &size, but there are things above
        // it. The 1024 is supposed to compensate this,
        // but is somewhat arbitrary.
        // 系统栈需要手动设置栈的高低边界
        size := gp.stack.hi
        if size == 0 {
            // 栈的大小默认设置为 8192 * sys.StackGuardMultiplier
            size = 8192 * sys.StackGuardMultiplier
        }
        // 高地址边界 gp.stack.hi 设置为当前栈的高地址。
        gp.stack.hi = uintptr(noescape(unsafe.Pointer(&size)))
        // 调整低地址边界 gp.stack.lo。
        gp.stack.lo = gp.stack.hi - size + 1024
    }
    // Initialize stack guard so that we can start calling regular
    // Go code.
    // 初始化堆栈保护，以便我们可以开始调用常规 Go 代码。
    // 计算栈的保护边界
    // gp.stackguard0 和 gp.stackguard1 都设置为距离栈底一定距离（_StackGuard）的位置，
    // 这保证了在栈空间不足时能够检测到栈溢出并处理。
    gp.stackguard0 = gp.stack.lo + _StackGuard
    // This is the g0, so we can also call go:systemstack
    // functions, which check stackguard1.
    // 这是 g0，因此我们还可以调用 go:systemstack 函数来检查 stackguard1。
    gp.stackguard1 = gp.stackguard0
    // 启动
    mstart1()

    // Exit this thread.
    if mStackIsSystemAllocated() {
        // Windows, Solaris, illumos, Darwin, AIX and Plan 9 always system-allocate
        // the stack, but put it in gp.stack before mstart,
        // so the logic above hasn't set osStack yet.
        // 根据系统栈的情况，推出线程
        osStack = true
    }
    // 退出线程
    mexit(osStack)
}

// The go:noinline is to guarantee the getcallerpc/getcallersp below are safe,
// so that we can set up g0.sched to return to the call of mstart1 above.
//
//go:noinline
func mstart1() {
    gp := getg()

    // 检查是否为g0
    if gp != gp.m.g0 {
        throw("bad runtime·mstart")
    }

    // Set up m.g0.sched as a label returning to just
    // after the mstart1 call in mstart0 above, for use by goexit0 and mcall.
    // We're never coming back to mstart1 after we call schedule,
    // so other calls can reuse the current frame.
    // And goexit0 does a gogo that needs to return from mstart1
    // and let mstart0 exit the thread.
    // 为了在 mcall 的栈顶使用调用方来结束当前线程，做记录
    // 当进入 schedule 之后，我们再也不会回到 mstart1，所以其他调用可以复用当前帧。
    // 设置 g0 的 sched 字段（调度信息），以便稍后在需要时可以从当前的栈帧返回
    // g0.sched.pc 设置为调用 mstart1 时的调用地址。
    // g0.sched.sp 设置为当前栈的指针。
    // 这些设置确保在 goexit0 或 mcall 函数调用时，M 可以正确地返回并让 mstart0 退出当前线程。
    // 保存当前运行现场
    gp.sched.g = guintptr(unsafe.Pointer(gp))
    gp.sched.pc = getcallerpc()
    gp.sched.sp = getcallersp()

    // 汇编层面的初始化工作
    asminit()
    // minit() 是用于初始化 M 的平台相关设置，确保当前线程可以处理 Go 运行时所需的环境和信号。
    minit()

    // Install signal handlers; after minit so that minit can
    // prepare the thread to be able to handle the signals.
    // 设置信号 handler；在 minit 之后，因为 minit 可以准备处理信号的的线程
    // 处理信号处理程序 (mstartm0)：
    if gp.m == &m0 {
        mstartm0()
    }

    // 执行启动函数
    // 一个可选的初始化函数，可以在 M 启动时执行。
    if fn := gp.m.mstartfn; fn != nil {
        fn()
    }

    // 如果当前 m 并非 m0，则要求绑定 p
    if gp.m != &m0 {
        // 绑定 p
        acquirep(gp.m.nextp.ptr())
        gp.m.nextp = 0
    }
    // 彻底准备好，开始调度，永不返回
    // 进入 Go 的调度循环。调度器会选择待执行的 Goroutine 并在当前的 M 上运行它。
    // 这是一个死循环，执行后永不返回，因此 M 将一直在调度 Goroutine。
    schedule()
}

// mstartm0 implements part of mstart1 that only runs on the m0.
//
// Write barriers are allowed here because we know the GC can't be
// running yet, so they'll be no-ops.
//
//go:yeswritebarrierrec
func mstartm0() {
    // Create an extra M for callbacks on threads not created by Go.
    // An extra M is also needed on Windows for callbacks created by
    // syscall.NewCallback. See issue #6751 for details.
    if (iscgo || GOOS == "windows") && !cgoHasExtraM {
        cgoHasExtraM = true
        newextram()
    }
    initsig(false)
}

// mPark causes a thread to park itself, returning once woken.
//
//go:nosplit
func mPark() {
    gp := getg()
    notesleep(&gp.m.park)
    noteclear(&gp.m.park)
}

// mexit tears down and exits the current thread.
//
// Don't call this directly to exit the thread, since it must run at
// the top of the thread stack. Instead, use gogo(&gp.m.g0.sched) to
// unwind the stack to the point that exits the thread.
//
// It is entered with m.p != nil, so write barriers are allowed. It
// will release the P before exiting.
// mexit 销毁并退出当前线程
//
// 请不要直接调用来退出线程，因为它必须在线程栈顶上运行。
// 相反，请使用 gogo(&_g_.m.g0.sched) 来解除栈并退出线程。
//
// 当调用时，m.p != nil。因此可以使用 write barrier。
// 在退出前它会释放当前绑定的 P。
//
// 只有 linux 中才可能正常的退出一个栈，而 darwin 只能保持暂止了。 而如果是主线程，则会始终保持 park。
//go:yeswritebarrierrec
func mexit(osStack bool) {
    mp := getg().m

    if mp == &m0 {
        // This is the main thread. Just wedge it.
        //
        // On Linux, exiting the main thread puts the process
        // into a non-waitable zombie state. On Plan 9,
        // exiting the main thread unblocks wait even though
        // other threads are still running. On Solaris we can
        // neither exitThread nor return from mstart. Other
        // bad things probably happen on other platforms.
        //
        // We could try to clean up this M more before wedging
        // it, but that complicates signal handling.
        // 主线程
        //
        // 在 linux 中，退出主线程会导致进程变为僵尸进程。
        // 在 plan 9 中，退出主线程将取消阻塞等待，即使其他线程仍在运行。
        // 在 Solaris 中我们既不能 exitThread 也不能返回到 mstart 中。
        // 其他系统上可能发生别的糟糕的事情。
        //
        // 我们可以尝试退出之前清理当前 M ，但信号处理非常复杂
        handoffp(releasep()) // 让出 P
        lock(&sched.lock)    // 锁住调度器
        sched.nmfreed++
        checkdead()
        unlock(&sched.lock)
        mPark() // 暂止主线程，在此阻塞
        throw("locked m0 woke up")
    }

    sigblock(true)
    unminit()

    // Free the gsignal stack.
    // 释放 gsignal 栈
    if mp.gsignal != nil {
        stackfree(mp.gsignal.stack)
        // On some platforms, when calling into VDSO (e.g. nanotime)
        // we store our g on the gsignal stack, if there is one.
        // Now the stack is freed, unlink it from the m, so we
        // won't write to it when calling VDSO code.
        mp.gsignal = nil
    }

    // Remove m from allm.
    // 将 m 从 allm 中移除
    lock(&sched.lock)
    for pprev := &allm; *pprev != nil; pprev = &(*pprev).alllink {
        if *pprev == mp {
            *pprev = mp.alllink
            goto found
        }
    }
    // 如果没找到则是异常状态，说明 allm 管理出错
    throw("m not found in allm")
found:
    // Delay reaping m until it's done with the stack.
    //
    // Put mp on the free list, though it will not be reaped while freeWait
    // is freeMWait. mp is no longer reachable via allm, so even if it is
    // on an OS stack, we must keep a reference to mp alive so that the GC
    // doesn't free mp while we are still using it.
    //
    // Note that the free list must not be linked through alllink because
    // some functions walk allm without locking, so may be using alllink.
    mp.freeWait.Store(freeMWait)
    mp.freelink = sched.freem
    sched.freem = mp
    unlock(&sched.lock)

    atomic.Xadd64(&ncgocall, int64(mp.ncgocall))

    // Release the P.
    handoffp(releasep())
    // After this point we must not have write barriers.

    // Invoke the deadlock detector. This must happen after
    // handoffp because it may have started a new M to take our
    // P's work.
    lock(&sched.lock)
    sched.nmfreed++
    checkdead()
    unlock(&sched.lock)

    if GOOS == "darwin" || GOOS == "ios" {
        // Make sure pendingPreemptSignals is correct when an M exits.
        // For #41702.
        if mp.signalPending.Load() != 0 {
            pendingPreemptSignals.Add(-1)
        }
    }

    // Destroy all allocated resources. After this is called, we may no
    // longer take any locks.
    // 销毁所有分配的资源。调用此方法后，我们可能不再使用任何锁。
    mdestroy(mp)

    if osStack {
        // No more uses of mp, so it is safe to drop the reference.
        mp.freeWait.Store(freeMRef)

        // Return from mstart and let the system thread
        // library free the g0 stack and terminate the thread.
        return
    }

    // mstart is the thread's entry point, so there's nothing to
    // return to. Exit the thread directly. exitThread will clear
    // m.freeWait when it's done with the stack and the m can be
    // reaped.
    exitThread(&mp.freeWait)
}

// forEachP calls fn(p) for every P p when p reaches a GC safe point.
// If a P is currently executing code, this will bring the P to a GC
// safe point and execute fn on that P. If the P is not executing code
// (it is idle or in a syscall), this will call fn(p) directly while
// preventing the P from exiting its state. This does not ensure that
// fn will run on every CPU executing Go code, but it acts as a global
// memory barrier. GC uses this as a "ragged barrier."
//
// The caller must hold worldsema.
//
//go:systemstack
func forEachP(fn func(*p)) {
    mp := acquirem()
    pp := getg().m.p.ptr()

    lock(&sched.lock)
    if sched.safePointWait != 0 {
        throw("forEachP: sched.safePointWait != 0")
    }
    sched.safePointWait = gomaxprocs - 1
    sched.safePointFn = fn

    // Ask all Ps to run the safe point function.
    for _, p2 := range allp {
        if p2 != pp {
            atomic.Store(&p2.runSafePointFn, 1)
        }
    }
    preemptall()

    // Any P entering _Pidle or _Psyscall from now on will observe
    // p.runSafePointFn == 1 and will call runSafePointFn when
    // changing its status to _Pidle/_Psyscall.

    // Run safe point function for all idle Ps. sched.pidle will
    // not change because we hold sched.lock.
    for p := sched.pidle.ptr(); p != nil; p = p.link.ptr() {
        if atomic.Cas(&p.runSafePointFn, 1, 0) {
            fn(p)
            sched.safePointWait--
        }
    }

    wait := sched.safePointWait > 0
    unlock(&sched.lock)

    // Run fn for the current P.
    fn(pp)

    // Force Ps currently in _Psyscall into _Pidle and hand them
    // off to induce safe point function execution.
    for _, p2 := range allp {
        s := p2.status
        if s == _Psyscall && p2.runSafePointFn == 1 && atomic.Cas(&p2.status, s, _Pidle) {
            if trace.enabled {
                traceGoSysBlock(p2)
                traceProcStop(p2)
            }
            p2.syscalltick++
            handoffp(p2)
        }
    }

    // Wait for remaining Ps to run fn.
    if wait {
        for {
            // Wait for 100us, then try to re-preempt in
            // case of any races.
            //
            // Requires system stack.
            if notetsleep(&sched.safePointNote, 100*1000) {
                noteclear(&sched.safePointNote)
                break
            }
            preemptall()
        }
    }
    if sched.safePointWait != 0 {
        throw("forEachP: not done")
    }
    for _, p2 := range allp {
        if p2.runSafePointFn != 0 {
            throw("forEachP: P did not run fn")
        }
    }

    lock(&sched.lock)
    sched.safePointFn = nil
    unlock(&sched.lock)
    releasem(mp)
}

// runSafePointFn runs the safe point function, if any, for this P.
// This should be called like
//
//	if getg().m.p.runSafePointFn != 0 {
//	    runSafePointFn()
//	}
//
// runSafePointFn must be checked on any transition in to _Pidle or
// _Psyscall to avoid a race where forEachP sees that the P is running
// just before the P goes into _Pidle/_Psyscall and neither forEachP
// nor the P run the safe-point function.
func runSafePointFn() {
    p := getg().m.p.ptr()
    // Resolve the race between forEachP running the safe-point
    // function on this P's behalf and this P running the
    // safe-point function directly.
    if !atomic.Cas(&p.runSafePointFn, 1, 0) {
        return
    }
    sched.safePointFn(p)
    lock(&sched.lock)
    sched.safePointWait--
    if sched.safePointWait == 0 {
        notewakeup(&sched.safePointNote)
    }
    unlock(&sched.lock)
}

// When running with cgo, we call _cgo_thread_start
// to start threads for us so that we can play nicely with
// foreign code.
var cgoThreadStart unsafe.Pointer

type cgothreadstart struct {
    g   guintptr
    tls *uint64
    fn  unsafe.Pointer
}

// Allocate a new m unassociated with any thread.
// Can use p for allocation context if needed.
// fn is recorded as the new m's m.mstartfn.
// id is optional pre-allocated m ID. Omit by passing -1.
//
// This function is allowed to have write barriers even if the caller
// isn't because it borrows pp.
//
//go:yeswritebarrierrec
func allocm(pp *p, fn func(), id int64) *m {
    allocmLock.rlock()

    // The caller owns pp, but we may borrow (i.e., acquirep) it. We must
    // disable preemption to ensure it is not stolen, which would make the
    // caller lose ownership.
    acquirem()

    gp := getg()
    // P 代表处理器资源，M 需要一个 P 来执行任务。如果当前的 M 没有绑定 P，则临时获取一个 P，用于内存分配等操作。
    if gp.m.p == 0 {
        acquirep(pp) // temporarily borrow p for mallocs in this function
    }

    // Release the free M list. We need to do this somewhere and
    // this may free up a stack we can use.
    // 如果调度器中有空闲的 M，则会尝试释放这些空闲的 M，特别是那些不再需要的 M 的栈空间。
    // stackfree 函数会释放 M 的 g0 栈（g0 是 M 的系统 Goroutine）。
    if sched.freem != nil {
        lock(&sched.lock)
        var newList *m
        for freem := sched.freem; freem != nil; {
            wait := freem.freeWait.Load()
            if wait == freeMWait {
                next := freem.freelink
                freem.freelink = newList
                newList = freem
                freem = next
                continue
            }
            // Free the stack if needed. For freeMRef, there is
            // nothing to do except drop freem from the sched.freem
            // list.
            if wait == freeMStack {
                // stackfree must be on the system stack, but allocm is
                // reachable off the system stack transitively from
                // startm.
                systemstack(func() {
                    stackfree(freem.g0.stack)
                })
            }
            freem = freem.freelink
        }
        sched.freem = newList
        unlock(&sched.lock)
    }

    // 创建新的M
    mp := new(m)
    mp.mstartfn = fn
    mcommoninit(mp, id)

    // In case of cgo or Solaris or illumos or Darwin, pthread_create will make us a stack.
    // Windows and Plan 9 will layout sched stack on OS stack.
    // 分配 g0 栈
    // g0 是每个 M 的特殊 Goroutine，负责执行调度相关的工作。
    // 如果是 cgo 或特定平台（Solaris、illumos、Darwin），栈会由 pthread_create 创建。否则，Go 会手动为 g0 分配栈。
    if iscgo || mStackIsSystemAllocated() {
        mp.g0 = malg(-1)
    } else {
        mp.g0 = malg(8192 * sys.StackGuardMultiplier)
    }
    mp.g0.m = mp

    // 释放P和M
    if pp == gp.m.p.ptr() {
        releasep()
    }

    releasem(gp.m)
    allocmLock.runlock()
    return mp
}

// needm is called when a cgo callback happens on a
// thread without an m (a thread not created by Go).
// In this case, needm is expected to find an m to use
// and return with m, g initialized correctly.
// Since m and g are not set now (likely nil, but see below)
// needm is limited in what routines it can call. In particular
// it can only call nosplit functions (textflag 7) and cannot
// do any scheduling that requires an m.
//
// In order to avoid needing heavy lifting here, we adopt
// the following strategy: there is a stack of available m's
// that can be stolen. Using compare-and-swap
// to pop from the stack has ABA races, so we simulate
// a lock by doing an exchange (via Casuintptr) to steal the stack
// head and replace the top pointer with MLOCKED (1).
// This serves as a simple spin lock that we can use even
// without an m. The thread that locks the stack in this way
// unlocks the stack by storing a valid stack head pointer.
//
// In order to make sure that there is always an m structure
// available to be stolen, we maintain the invariant that there
// is always one more than needed. At the beginning of the
// program (if cgo is in use) the list is seeded with a single m.
// If needm finds that it has taken the last m off the list, its job
// is - once it has installed its own m so that it can do things like
// allocate memory - to create a spare m and put it on the list.
//
// Each of these extra m's also has a g0 and a curg that are
// pressed into service as the scheduling stack and current
// goroutine for the duration of the cgo callback.
//
// When the callback is done with the m, it calls dropm to
// put the m back on the list.
//
//go:nosplit
func needm() {
    if (iscgo || GOOS == "windows") && !cgoHasExtraM {
        // Can happen if C/C++ code calls Go from a global ctor.
        // Can also happen on Windows if a global ctor uses a
        // callback created by syscall.NewCallback. See issue #6751
        // for details.
        //
        // Can not throw, because scheduler is not initialized yet.
        writeErrStr("fatal error: cgo callback before cgo call\n")
        exit(1)
    }

    // Save and block signals before getting an M.
    // The signal handler may call needm itself,
    // and we must avoid a deadlock. Also, once g is installed,
    // any incoming signals will try to execute,
    // but we won't have the sigaltstack settings and other data
    // set up appropriately until the end of minit, which will
    // unblock the signals. This is the same dance as when
    // starting a new m to run Go code via newosproc.
    var sigmask sigset
    sigsave(&sigmask)
    sigblock(false)

    // Lock extra list, take head, unlock popped list.
    // nilokay=false is safe here because of the invariant above,
    // that the extra list always contains or will soon contain
    // at least one m.
    mp := lockextra(false)

    // Set needextram when we've just emptied the list,
    // so that the eventual call into cgocallbackg will
    // allocate a new m for the extra list. We delay the
    // allocation until then so that it can be done
    // after exitsyscall makes sure it is okay to be
    // running at all (that is, there's no garbage collection
    // running right now).
    mp.needextram = mp.schedlink == 0
    extraMCount--
    unlockextra(mp.schedlink.ptr())

    // Store the original signal mask for use by minit.
    mp.sigmask = sigmask

    // Install TLS on some platforms (previously setg
    // would do this if necessary).
    osSetupTLS(mp)

    // Install g (= m->g0) and set the stack bounds
    // to match the current stack. We don't actually know
    // how big the stack is, like we don't know how big any
    // scheduling stack is, but we assume there's at least 32 kB,
    // which is more than enough for us.
    setg(mp.g0)
    gp := getg()
    gp.stack.hi = getcallersp() + 1024
    gp.stack.lo = getcallersp() - 32*1024
    gp.stackguard0 = gp.stack.lo + _StackGuard

    // Initialize this thread to use the m.
    asminit()
    minit()

    // mp.curg is now a real goroutine.
    casgstatus(mp.curg, _Gdead, _Gsyscall)
    sched.ngsys.Add(-1)
}

// newextram allocates m's and puts them on the extra list.
// It is called with a working local m, so that it can do things
// like call schedlock and allocate.
func newextram() {
    c := extraMWaiters.Swap(0)
    if c > 0 {
        for i := uint32(0); i < c; i++ {
            oneNewExtraM()
        }
    } else {
        // Make sure there is at least one extra M.
        mp := lockextra(true)
        unlockextra(mp)
        if mp == nil {
            oneNewExtraM()
        }
    }
}

// oneNewExtraM allocates an m and puts it on the extra list.
func oneNewExtraM() {
    // Create extra goroutine locked to extra m.
    // The goroutine is the context in which the cgo callback will run.
    // The sched.pc will never be returned to, but setting it to
    // goexit makes clear to the traceback routines where
    // the goroutine stack ends.
    mp := allocm(nil, nil, -1)
    gp := malg(4096)
    gp.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum
    gp.sched.sp = gp.stack.hi
    gp.sched.sp -= 4 * goarch.PtrSize // extra space in case of reads slightly beyond frame
    gp.sched.lr = 0
    gp.sched.g = guintptr(unsafe.Pointer(gp))
    gp.syscallpc = gp.sched.pc
    gp.syscallsp = gp.sched.sp
    gp.stktopsp = gp.sched.sp
    // malg returns status as _Gidle. Change to _Gdead before
    // adding to allg where GC can see it. We use _Gdead to hide
    // this from tracebacks and stack scans since it isn't a
    // "real" goroutine until needm grabs it.
    casgstatus(gp, _Gidle, _Gdead)
    gp.m = mp
    mp.curg = gp
    mp.isextra = true
    mp.lockedInt++
    mp.lockedg.set(gp)
    gp.lockedm.set(mp)
    gp.goid = sched.goidgen.Add(1)
    gp.sysblocktraced = true
    if raceenabled {
        gp.racectx = racegostart(abi.FuncPCABIInternal(newextram) + sys.PCQuantum)
    }
    if trace.enabled {
        // Trigger two trace events for the locked g in the extra m,
        // since the next event of the g will be traceEvGoSysExit in exitsyscall,
        // while calling from C thread to Go.
        traceGoCreate(gp, 0) // no start pc
        gp.traceseq++
        traceEvent(traceEvGoInSyscall, -1, gp.goid)
    }
    // put on allg for garbage collector
    allgadd(gp)

    // gp is now on the allg list, but we don't want it to be
    // counted by gcount. It would be more "proper" to increment
    // sched.ngfree, but that requires locking. Incrementing ngsys
    // has the same effect.
    sched.ngsys.Add(1)

    // Add m to the extra list.
    mnext := lockextra(true)
    mp.schedlink.set(mnext)
    extraMCount++
    unlockextra(mp)
}

// dropm is called when a cgo callback has called needm but is now
// done with the callback and returning back into the non-Go thread.
// It puts the current m back onto the extra list.
//
// The main expense here is the call to signalstack to release the
// m's signal stack, and then the call to needm on the next callback
// from this thread. It is tempting to try to save the m for next time,
// which would eliminate both these costs, but there might not be
// a next time: the current thread (which Go does not control) might exit.
// If we saved the m for that thread, there would be an m leak each time
// such a thread exited. Instead, we acquire and release an m on each
// call. These should typically not be scheduling operations, just a few
// atomics, so the cost should be small.
//
// TODO(rsc): An alternative would be to allocate a dummy pthread per-thread
// variable using pthread_key_create. Unlike the pthread keys we already use
// on OS X, this dummy key would never be read by Go code. It would exist
// only so that we could register at thread-exit-time destructor.
// That destructor would put the m back onto the extra list.
// This is purely a performance optimization. The current version,
// in which dropm happens on each cgo call, is still correct too.
// We may have to keep the current version on systems with cgo
// but without pthreads, like Windows.
func dropm() {
    // Clear m and g, and return m to the extra list.
    // After the call to setg we can only call nosplit functions
    // with no pointer manipulation.
    mp := getg().m

    // Return mp.curg to dead state.
    casgstatus(mp.curg, _Gsyscall, _Gdead)
    mp.curg.preemptStop = false
    sched.ngsys.Add(1)

    // Block signals before unminit.
    // Unminit unregisters the signal handling stack (but needs g on some systems).
    // Setg(nil) clears g, which is the signal handler's cue not to run Go handlers.
    // It's important not to try to handle a signal between those two steps.
    sigmask := mp.sigmask
    sigblock(false)
    unminit()

    mnext := lockextra(true)
    extraMCount++
    mp.schedlink.set(mnext)

    setg(nil)

    // Commit the release of mp.
    unlockextra(mp)

    msigrestore(sigmask)
}

// A helper function for EnsureDropM.
func getm() uintptr {
    return uintptr(unsafe.Pointer(getg().m))
}

var extram atomic.Uintptr
var extraMCount uint32 // Protected by lockextra
var extraMWaiters atomic.Uint32

// lockextra locks the extra list and returns the list head.
// The caller must unlock the list by storing a new list head
// to extram. If nilokay is true, then lockextra will
// return a nil list head if that's what it finds. If nilokay is false,
// lockextra will keep waiting until the list head is no longer nil.
//
//go:nosplit
func lockextra(nilokay bool) *m {
    const locked = 1

    incr := false
    for {
        old := extram.Load()
        if old == locked {
            osyield_no_g()
            continue
        }
        if old == 0 && !nilokay {
            if !incr {
                // Add 1 to the number of threads
                // waiting for an M.
                // This is cleared by newextram.
                extraMWaiters.Add(1)
                incr = true
            }
            usleep_no_g(1)
            continue
        }
        if extram.CompareAndSwap(old, locked) {
            return (*m)(unsafe.Pointer(old))
        }
        osyield_no_g()
        continue
    }
}

//go:nosplit
func unlockextra(mp *m) {
    extram.Store(uintptr(unsafe.Pointer(mp)))
}

var (
    // allocmLock is locked for read when creating new Ms in allocm and their
    // addition to allm. Thus acquiring this lock for write blocks the
    // creation of new Ms.
    allocmLock rwmutex

    // execLock serializes exec and clone to avoid bugs or unspecified
    // behaviour around exec'ing while creating/destroying threads. See
    // issue #19546.
    execLock rwmutex
)

// These errors are reported (via writeErrStr) by some OS-specific
// versions of newosproc and newosproc0.
const (
    failthreadcreate  = "runtime: failed to create new OS thread\n"
    failallocatestack = "runtime: failed to allocate stack for the new OS thread\n"
)

// newmHandoff contains a list of m structures that need new OS threads.
// This is used by newm in situations where newm itself can't safely
// start an OS thread.
// newmHandoff 包含需要新 OS 线程的 m 的列表。
// 在 newm 本身无法安全启动 OS 线程的情况下，newm 会使用它。
var newmHandoff struct {
    lock mutex

    // newm points to a list of M structures that need new OS
    // threads. The list is linked through m.schedlink.
    // newm 指向需要新 OS 线程的M结构列表。 该列表通过 m.schedlink 链接。
    newm muintptr

    // waiting indicates that wake needs to be notified when an m
    // is put on the list.
    // waiting 表示当 m 列入列表时需要通知唤醒。
    waiting bool
    wake    note

    // haveTemplateThread indicates that the templateThread has
    // been started. This is not protected by lock. Use cas to set
    // to 1.
    // haveTemplateThread 表示 templateThread 已经启动。没有锁保护，使用 cas 设置为 1。
    haveTemplateThread uint32
}

// Create a new m. It will start off with a call to fn, or else the scheduler.
// fn needs to be static and not a heap allocated closure.
// May run with m.p==nil, so write barriers are not allowed.
//
// // 创建一个新的 m. 它会启动并调用 fn 或调度器
// // fn 必须是静态、非堆上分配的闭包
// // 它可能在 m.p==nil 时运行，因此不允许 write barrier
//
// id is optional pre-allocated m ID. Omit by passing -1.
//
//go:nowritebarrierrec
func newm(fn func(), pp *p, id int64) {
    // allocm adds a new M to allm, but they do not start until created by
    // the OS in newm1 or the template thread.
    //
    // doAllThreadsSyscall requires that every M in allm will eventually
    // start and be signal-able, even with a STW.
    //
    // Disable preemption here until we start the thread to ensure that
    // newm is not preempted between allocm and starting the new thread,
    // ensuring that anything added to allm is guaranteed to eventually
    // start.
    acquirem()

    // 分配一个M
    mp := allocm(pp, fn, id)
    // 设置 p 用于后续绑定
    mp.nextp.set(pp)
    // 设置 signal mask
    mp.sigmask = initSigmask
    if gp := getg(); gp != nil && gp.m != nil && (gp.m.lockedExt != 0 || gp.m.incgo) && GOOS != "plan9" {
        // We're on a locked M or a thread that may have been
        // started by C. The kernel state of this thread may
        // be strange (the user may have locked it for that
        // purpose). We don't want to clone that into another
        // thread. Instead, ask a known-good thread to create
        // the thread for us.
        //
        // 我们处于一个锁定的 M 或可能由 C 启动的线程。这个线程的内核状态可能
        // 很奇怪（用户可能已将其锁定）。我们不想将其克隆到另一个线程。
        // 相反，请求一个已知状态良好的线程来创建给我们的线程。
        // This is disabled on Plan 9. See golang.org/issue/22227.
        //
        // TODO: This may be unnecessary on Windows, which
        // doesn't model thread creation off fork.
        lock(&newmHandoff.lock)
        if newmHandoff.haveTemplateThread == 0 {
            throw("on a locked thread with no template thread")
        }
        mp.schedlink = newmHandoff.newm
        newmHandoff.newm.set(mp)
        if newmHandoff.waiting {
            newmHandoff.waiting = false
            // 唤醒 m, 自旋到非自旋
            notewakeup(&newmHandoff.wake)
        }
        unlock(&newmHandoff.lock)
        // The M has not started yet, but the template thread does not
        // participate in STW, so it will always process queued Ms and
        // it is safe to releasem.
        releasem(getg().m)
        return
    }
    newm1(mp)
    releasem(getg().m)
}

func newm1(mp *m) {
    if iscgo {
        var ts cgothreadstart
        if _cgo_thread_start == nil {
            throw("_cgo_thread_start missing")
        }
        ts.g.set(mp.g0)
        ts.tls = (*uint64)(unsafe.Pointer(&mp.tls[0]))
        ts.fn = unsafe.Pointer(abi.FuncPCABI0(mstart))
        if msanenabled {
            msanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
        }
        if asanenabled {
            asanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
        }
        execLock.rlock() // Prevent process clone.
        asmcgocall(_cgo_thread_start, unsafe.Pointer(&ts))
        execLock.runlock()
        return
    }
    execLock.rlock() // Prevent process clone.
    newosproc(mp)
    execLock.runlock()
}

// startTemplateThread starts the template thread if it is not already
// running.
//
// The calling thread must itself be in a known-good state.
// 如果模板线程尚未运行，则startTemplateThread将启动它。
//
// 调用线程本身必须处于已知良好状态。
func startTemplateThread() {
    if GOARCH == "wasm" { // no threads on wasm yet
        return
    }

    // Disable preemption to guarantee that the template thread will be
    // created before a park once haveTemplateThread is set.
    mp := acquirem()
    if !atomic.Cas(&newmHandoff.haveTemplateThread, 0, 1) {
        releasem(mp)
        return
    }
    newm(templateThread, nil, -1)
    releasem(mp)
}

// templateThread is a thread in a known-good state that exists solely
// to start new threads in known-good states when the calling thread
// may not be in a good state.
//
// Many programs never need this, so templateThread is started lazily
// when we first enter a state that might lead to running on a thread
// in an unknown state.
//
// templateThread runs on an M without a P, so it must not have write
// barriers.
//
// templateThread是处于已知良好状态的线程，仅当调用线程可能不是良好状态时，
// 该线程仅用于在已知良好状态下启动新线程。
//
// 许多程序不需要这个，所以当我们第一次进入可能导致在未知状态的线程上运行的状态时，
// templateThread会懒启动。
//
// templateThread 在没有 P 的 M 上运行，因此它必须没有写障碍。
//
// 模版线程本身不会退出，只会在需要的时候创建M
//go:nowritebarrierrec
func templateThread() {
    lock(&sched.lock)
    sched.nmsys++
    checkdead()
    unlock(&sched.lock)

    for {
        lock(&newmHandoff.lock)
        for newmHandoff.newm != 0 {
            newm := newmHandoff.newm.ptr()
            newmHandoff.newm = 0
            unlock(&newmHandoff.lock)
            for newm != nil {
                next := newm.schedlink.ptr()
                newm.schedlink = 0
                newm1(newm)
                newm = next
            }
            lock(&newmHandoff.lock)
        }
        // 等待新的创建请求
        newmHandoff.waiting = true
        noteclear(&newmHandoff.wake)
        unlock(&newmHandoff.lock)
        // 创建好后模版线程会休眠
        notesleep(&newmHandoff.wake)
    }
}

// Stops execution of the current m until new work is available.
// Returns with acquired P.
// 停止当前 m 的执行，直到有新工作可用。
func stopm() {
    gp := getg()

    if gp.m.locks != 0 {
        throw("stopm holding locks")
    }
    if gp.m.p != 0 {
        throw("stopm holding p")
    }
    if gp.m.spinning {
        throw("stopm spinning")
    }

    // 将 m 放回到 空闲列表中，因为我们马上就要暂止了
    lock(&sched.lock)
    mput(gp.m)
    unlock(&sched.lock)
    // 暂止当前的 M，在此阻塞，直到被唤醒
    mPark()
    // 此时已经被复始，说明有任务要执行
    // 立即 acquire P
    acquirep(gp.m.nextp.ptr())
    gp.m.nextp = 0
}

func mspinning() {
    // startm's caller incremented nmspinning. Set the new M's spinning.
    getg().m.spinning = true
}

// Schedules some M to run the p (creates an M if necessary).
// If p==nil, tries to get an idle P, if no idle P's does nothing.
// May run with m.p==nil, so write barriers are not allowed.
// If spinning is set, the caller has incremented nmspinning and must provide a
// P. startm will set m.spinning in the newly started M.
//
// Callers passing a non-nil P must call from a non-preemptible context. See
// comment on acquirem below.
//
// Argument lockheld indicates whether the caller already acquired the
// scheduler lock. Callers holding the lock when making the call must pass
// true. The lock might be temporarily dropped, but will be reacquired before
// returning.
//
// Must not have write barriers because this may be called without a P.
//
// startm 函数的目的是启动一个新的 M 来执行任务，或者为现有的 P 分配一个 M。
// 它可以在系统中没有空闲的 M 时创建新的 M，并将其与 P 关联，以继续调度并执行可运行的 G。
// 	1.	当有空闲的 P 但没有 M 时，它会创建新的 M。
//	2.	当系统中有任务需要处理时，它会唤醒 M 来执行这些任务。
//	3.	对于旋转 M，它会确保 M 处于正确的状态，并且没有冲突的 G 或 P。
//go:nowritebarrierrec
func startm(pp *p, spinning, lockheld bool) {
    // Disable preemption.
    //
    // Every owned P must have an owner that will eventually stop it in the
    // event of a GC stop request. startm takes transient ownership of a P
    // (either from argument or pidleget below) and transfers ownership to
    // a started M, which will be responsible for performing the stop.
    //
    // Preemption must be disabled during this transient ownership,
    // otherwise the P this is running on may enter GC stop while still
    // holding the transient P, leaving that P in limbo and deadlocking the
    // STW.
    //
    // Callers passing a non-nil P must already be in non-preemptible
    // context, otherwise such preemption could occur on function entry to
    // startm. Callers passing a nil P may be preemptible, so we must
    // disable preemption before acquiring a P from pidleget below.
    mp := acquirem()
    if !lockheld {
        lock(&sched.lock)
    }
    if pp == nil {
        // 获取P
        if spinning {
            // TODO(prattmic): All remaining calls to this function
            // with _p_ == nil could be cleaned up to find a P
            // before calling startm.
            throw("startm: P required for spinning=true")
        }
        // 从空闲列表获取，没有就返回
        pp, _ = pidleget(0)
        if pp == nil {
            if !lockheld {
                unlock(&sched.lock)
            }
            releasem(mp)
            return
        }
    }
    // 尝试获取一个空闲的M
    nmp := mget()
    if nmp == nil {
        // No M is available, we must drop sched.lock and call newm.
        // However, we already own a P to assign to the M.
        //
        // Once sched.lock is released, another G (e.g., in a syscall),
        // could find no idle P while checkdead finds a runnable G but
        // no running M's because this new M hasn't started yet, thus
        // throwing in an apparent deadlock.
        // This apparent deadlock is possible when startm is called
        // from sysmon, which doesn't count as a running M.
        //
        // Avoid this situation by pre-allocating the ID for the new M,
        // thus marking it as 'running' before we drop sched.lock. This
        // new M will eventually run the scheduler to execute any
        // queued G's.
        // 没有获取到空闲的M，准备创建一个新的M与P关联
        id := mReserveID()
        unlock(&sched.lock)

        var fn func()
        if spinning {
            // The caller incremented nmspinning, so set m.spinning in the new M.
            fn = mspinning
        }
        newm(fn, pp, id)

        if lockheld {
            lock(&sched.lock)
        }
        // Ownership transfer of pp committed by start in newm.
        // Preemption is now safe.
        releasem(mp)
        return
    }
    if !lockheld {
        unlock(&sched.lock)
    }
    // 检查状态并设置 M
    if nmp.spinning {
        throw("startm: m is spinning")
    }
    if nmp.nextp != 0 {
        throw("startm: m has p")
    }
    if spinning && !runqempty(pp) {
        throw("startm: p has runnable gs")
    }
    // The caller incremented nmspinning, so set m.spinning in the new M.
    // 设置M和P关联
    nmp.spinning = spinning
    nmp.nextp.set(pp)
    // 唤醒M
    notewakeup(&nmp.park)
    // Ownership transfer of pp committed by wakeup. Preemption is now
    // safe.
    releasem(mp)
}

// Hands off P from syscall or locked M.
// Always runs without a P, so write barriers are not allowed.
//
//go:nowritebarrierrec
func handoffp(pp *p) {
    // handoffp must start an M in any situation where
    // findrunnable would return a G to run on pp.

    // if it has local work, start it straight away
    if !runqempty(pp) || sched.runqsize != 0 {
        startm(pp, false, false)
        return
    }
    // if there's trace work to do, start it straight away
    if (trace.enabled || trace.shutdown) && traceReaderAvailable() != nil {
        startm(pp, false, false)
        return
    }
    // if it has GC work, start it straight away
    if gcBlackenEnabled != 0 && gcMarkWorkAvailable(pp) {
        startm(pp, false, false)
        return
    }
    // no local work, check that there are no spinning/idle M's,
    // otherwise our help is not required
    if sched.nmspinning.Load()+sched.npidle.Load() == 0 && sched.nmspinning.CompareAndSwap(0, 1) { // TODO: fast atomic
        sched.needspinning.Store(0)
        startm(pp, true, false)
        return
    }
    lock(&sched.lock)
    if sched.gcwaiting.Load() {
        pp.status = _Pgcstop
        sched.stopwait--
        if sched.stopwait == 0 {
            notewakeup(&sched.stopnote)
        }
        unlock(&sched.lock)
        return
    }
    if pp.runSafePointFn != 0 && atomic.Cas(&pp.runSafePointFn, 1, 0) {
        sched.safePointFn(pp)
        sched.safePointWait--
        if sched.safePointWait == 0 {
            notewakeup(&sched.safePointNote)
        }
    }
    if sched.runqsize != 0 {
        unlock(&sched.lock)
        startm(pp, false, false)
        return
    }
    // If this is the last running P and nobody is polling network,
    // need to wakeup another M to poll network.
    if sched.npidle.Load() == gomaxprocs-1 && sched.lastpoll.Load() != 0 {
        unlock(&sched.lock)
        startm(pp, false, false)
        return
    }

    // The scheduler lock cannot be held when calling wakeNetPoller below
    // because wakeNetPoller may call wakep which may call startm.
    when := nobarrierWakeTime(pp)
    pidleput(pp, 0)
    unlock(&sched.lock)

    if when != 0 {
        wakeNetPoller(when)
    }
}

// Tries to add one more P to execute G's.
// Called when a G is made runnable (newproc, ready).
// Must be called with a P.
func wakep() {
    // Be conservative about spinning threads, only start one if none exist
    // already.
    if sched.nmspinning.Load() != 0 || !sched.nmspinning.CompareAndSwap(0, 1) {
        return
    }

    // Disable preemption until ownership of pp transfers to the next M in
    // startm. Otherwise preemption here would leave pp stuck waiting to
    // enter _Pgcstop.
    //
    // See preemption comment on acquirem in startm for more details.
    // 获取当前的 M，并禁止抢占。这样确保在 P 被分配给下一个 M 之前，不会被抢占。
    mp := acquirem()

    var pp *p
    lock(&sched.lock)
    // 查找空闲的P
    // 尝试从空闲 P 列表中获取一个 P。如果找不到空闲的 P，pp 为 nil。
    pp, _ = pidlegetSpinning(0)
    if pp == nil {
        // 如果 pp == nil，则没有空闲的 P，此时将 nmspinning 减 1，表示没有 M 处于旋转状态，然后解锁并释放当前 M。
        if sched.nmspinning.Add(-1) < 0 {
            throw("wakep: negative nmspinning")
        }
        unlock(&sched.lock)
        releasem(mp)
        return
    }
    // Since we always have a P, the race in the "No M is available"
    // comment in startm doesn't apply during the small window between the
    // unlock here and lock in startm. A checkdead in between will always
    // see at least one running M (ours).
    unlock(&sched.lock)

    // 如果找到空闲的 P，则调用 startm(pp, true, false) 启动一个新的 M 来执行 P
    startm(pp, true, false)

    // 解除对 M 的持有，恢复抢占。
    releasem(mp)
}

// Stops execution of the current m that is locked to a g until the g is runnable again.
// Returns with acquired P.
// 停止当前正在执行锁住的 g 的 m 的执行，直到 g 重新变为 runnable。
// 返回获得的 P
func stoplockedm() {
    gp := getg()

    if gp.m.lockedg == 0 || gp.m.lockedg.ptr().lockedm.ptr() != gp.m {
        throw("stoplockedm: inconsistent locking")
    }
    if gp.m.p != 0 {
        // Schedule another M to run this p.
        // 调度其他M来运行P
        pp := releasep()
        handoffp(pp)
    }
    incidlelocked(1)
    // Wait until another thread schedules lockedg again.
    // 等待直到其他线程可以再次调度 lockedg
    mPark()
    status := readgstatus(gp.m.lockedg.ptr())
    if status&^_Gscan != _Grunnable {
        print("runtime:stoplockedm: lockedg (atomicstatus=", status, ") is not Grunnable or Gscanrunnable\n")
        dumpgstatus(gp.m.lockedg.ptr())
        throw("stoplockedm: not runnable")
    }
    acquirep(gp.m.nextp.ptr())
    gp.m.nextp = 0
}

// Schedules the locked m to run the locked gp.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func startlockedm(gp *g) {
    mp := gp.lockedm.ptr()
    if mp == getg().m {
        throw("startlockedm: locked to me")
    }
    if mp.nextp != 0 {
        throw("startlockedm: m has p")
    }
    // directly handoff current P to the locked m
    incidlelocked(-1)
    pp := releasep()
    mp.nextp.set(pp)
    notewakeup(&mp.park)
    stopm()
}

// Stops the current m for stopTheWorld.
// Returns when the world is restarted.
func gcstopm() {
    gp := getg()

    if !sched.gcwaiting.Load() {
        throw("gcstopm: not waiting for gc")
    }
    if gp.m.spinning {
        gp.m.spinning = false
        // OK to just drop nmspinning here,
        // startTheWorld will unpark threads as necessary.
        if sched.nmspinning.Add(-1) < 0 {
            throw("gcstopm: negative nmspinning")
        }
    }
    pp := releasep()
    lock(&sched.lock)
    pp.status = _Pgcstop
    sched.stopwait--
    if sched.stopwait == 0 {
        notewakeup(&sched.stopnote)
    }
    unlock(&sched.lock)
    stopm()
}

// Schedules gp to run on the current M.
// If inheritTime is true, gp inherits the remaining time in the
// current time slice. Otherwise, it starts a new time slice.
// Never returns.
//
// Write barriers are allowed because this is called immediately after
// acquiring a P in several places.
//
//go:yeswritebarrierrec
// 在当前 M 上调度 gp。
// 如果 inheritTime 为 true，则 gp 继承剩余的时间片。否则从一个新的时间片开始
// 永不返回。
func execute(gp *g, inheritTime bool) {
    mp := getg().m

    if goroutineProfile.active {
        // Make sure that gp has had its stack written out to the goroutine
        // profile, exactly as it was when the goroutine profiler first stopped
        // the world.
        tryRecordGoroutineProfile(gp, osyield)
    }

    // Assign gp.m before entering _Grunning so running Gs have an
    // M.
    // // 将 g 正式切换为 _Grunning 状态
    mp.curg = gp
    gp.m = mp
    casgstatus(gp, _Grunnable, _Grunning)
    gp.waitsince = 0
    // 抢占信号
    gp.preempt = false
    gp.stackguard0 = gp.stack.lo + _StackGuard
    if !inheritTime {
        mp.p.ptr().schedtick++
    }

    // Check whether the profiler needs to be turned on or off.
    hz := sched.profilehz
    if mp.profilehz != hz {
        setThreadCPUProfiler(hz)
    }

    if trace.enabled {
        // GoSysExit has to happen when we have a P, but before GoStart.
        // So we emit it here.
        if gp.syscallsp != 0 && gp.sysblocktraced {
            traceGoSysExit(gp.sysexitticks)
        }
        traceGoStart()
    }

    // 开始执行
    gogo(&gp.sched)
}

// Finds a runnable goroutine to execute.
// Tries to steal from other P's, get g from local or global queue, poll network.
// tryWakeP indicates that the returned goroutine is not normal (GC worker, trace
// reader) so the caller should try to wake a P.
// 找到一个可运行的 goroutine 来执行。
// 尝试从其他 P 窃取，从本地或全局队列获取 g，轮询网络。
// inheritTime 是否继承调度时间
// tryWakeP 表示返回的 goroutine 不正常（GC 工作线程、跟踪读取器），因此调用者应尝试唤醒 P。
func findRunnable() (gp *g, inheritTime, tryWakeP bool) {
    mp := getg().m

    // The conditions here and in handoffp must agree: if
    // findrunnable would return a G to run, handoffp must start
    // an M.

top:
    pp := mp.p.ptr()
    if sched.gcwaiting.Load() {
        // 如果在 gc，则暂止当前 m，直到复始后回到 top
        gcstopm()
        goto top
    }
    // 如果 P 需要进入 Safe Point（安全点，通常在 GC 过程中），则运行相关的 Safe Point 函数。
    if pp.runSafePointFn != 0 {
        runSafePointFn()
    }

    // now and pollUntil are saved for work stealing later,
    // which may steal timers. It's important that between now
    // and then, nothing blocks, so these numbers remain mostly
    // relevant.
    // 检查是否有需要处理的定时器
    // 定时器机制可以让 goroutine 在指定时间后被唤醒，如果有到期的定时器任务，调度器会处理它们。
    now, pollUntil, _ := checkTimers(pp, 0)

    // Try to schedule the trace reader.
    if trace.enabled || trace.shutdown {
        gp := traceReader()
        if gp != nil {
            casgstatus(gp, _Gwaiting, _Grunnable)
            traceGoUnpark(gp, 0)
            return gp, false, true
        }
    }

    // Try to schedule a GC worker.
    // 如果当前正在执行 GC 并且有 GC 工作 goroutine 需要执行，则调度 GC worker。
    if gcBlackenEnabled != 0 {
        gp, tnow := gcController.findRunnableGCWorker(pp, now)
        if gp != nil {
            return gp, false, true
        }
        now = tnow
    }

    // Check the global runnable queue once in a while to ensure fairness.
    // Otherwise two goroutines can completely occupy the local runqueue
    // by constantly respawning each other.
    // 每调度 61 次，就检查一次全局队列，保证公平性
    // 否则两个 Goroutine 可以通过互相 respawn 一直占领本地的 runqueue
    // 检查全局运行队列中的 goroutine
    // 全局运行队列保存了所有 M 和 P 之间可共享的 goroutine，每 61 次调度时会检查全局运行队列。
    if pp.schedtick%61 == 0 && sched.runqsize > 0 {
        lock(&sched.lock)
        gp := globrunqget(pp, 1)
        unlock(&sched.lock)
        if gp != nil {
            return gp, false, false
        }
    }

    // Wake up the finalizer G.
    if fingStatus.Load()&(fingWait|fingWake) == fingWait|fingWake {
        if gp := wakefing(); gp != nil {
            ready(gp, 0, true)
        }
    }
    // cgo 调用被终止，继续进入
    if *cgo_yield != nil {
        asmcgocall(*cgo_yield, nil)
    }

    // local runq
    // 尝试从本地队列获取协程
    if gp, inheritTime := runqget(pp); gp != nil {
        return gp, inheritTime, false
    }

    // global runq
    // 尝试从全局队列获取协程
    if sched.runqsize != 0 {
        lock(&sched.lock)
        gp := globrunqget(pp, 0)
        unlock(&sched.lock)
        if gp != nil {
            return gp, false, false
        }
    }

    // Poll network.
    // This netpoll is only an optimization before we resort to stealing.
    // We can safely skip it if there are no waiters or a thread is blocked
    // in netpoll already. If there is any kind of logical race with that
    // blocked thread (e.g. it has already returned from netpoll, but does
    // not set lastpoll yet), this thread will do blocking netpoll below
    // anyway.
    // Poll 网络，优先级比从其他 P 中偷要高。
    // 在我们尝试去其他 P 偷之前，这个 netpoll 只是一个优化。
    // 如果没有 waiter 或 netpoll 中的线程已被阻塞，则可以安全地跳过它。
    // 如果有任何类型的逻辑竞争与被阻塞的线程（例如它已经从 netpoll 返回，但尚未设置 lastpoll）
    // 该线程无论如何都将阻塞 netpoll。
    // 如果网络轮询初始化了，并且有 goroutine 等待网络事件，调度器会调用 netpoll 以非阻塞的方式检查是否有可执行的网络事件。
    // 如果当前没有找到工作且已经初始化了网络轮询机制，调度器会尝试通过网络轮询获取新任务
    if netpollinited() && netpollWaiters.Load() > 0 && sched.lastpoll.Load() != 0 {
        // netpoll(0) 是非阻塞轮询操作，它会检查网络事件并返回等待执行的 goroutine 列表。
        if list := netpoll(0); !list.empty() { // non-blocking
            // 如果有等待处理的网络事件，调度器会将其状态设置为可运行并返回该 goroutine 进行调度。
            gp := list.pop()
            injectglist(&list)
            casgstatus(gp, _Gwaiting, _Grunnable)
            if trace.enabled {
                traceGoUnpark(gp, 0)
            }
            return gp, false, false
        }
    }

    // Spinning Ms: steal work from other Ps.
    //
    // Limit the number of spinning Ms to half the number of busy Ps.
    // This is necessary to prevent excessive CPU consumption when
    // GOMAXPROCS>>1 but the program parallelism is low.
    // 自旋的M的工作窃取
    // nmspinning：记录当前有多少 M 处于旋转状态。为了避免过多 CPU 资源浪费，旋转的 M 的数量被限制在忙碌的 P 的一半（GOMAXPROCS/2）。
    if mp.spinning || 2*sched.nmspinning.Load() < gomaxprocs-sched.npidle.Load() {
        if !mp.spinning {
            mp.becomeSpinning()
        }

        // 从其他P窃取工作
        gp, inheritTime, tnow, w, newWork := stealWork(now)
        if gp != nil {
            // Successfully stole.
            return gp, inheritTime, false
        }
        if newWork {
            // There may be new timer or GC work; restart to
            // discover.
            // 如果有新的定时器或 GC 工作，则重新开始寻找工作
            goto top
        }

        now = tnow
        if w != 0 && (pollUntil == 0 || w < pollUntil) {
            // Earlier timer to wait for.
            // 发现了一个更早的定时器
            pollUntil = w
        }
    }

    // 空闲 GC Worker 的调度
    // We have nothing to do.
    //
    // If we're in the GC mark phase, can safely scan and blacken objects,
    // and have work to do, run idle-time marking rather than give up the P.
    // 如果垃圾回收（GC）的黑化阶段已经启用并且有工作可做，
    // 调度器会启动一个空闲 GC 工作线程来执行 GC 相关的任务。
    if gcBlackenEnabled != 0 && gcMarkWorkAvailable(pp) && gcController.addIdleMarkWorker() {
        // 如果 gcController.addIdleMarkWorker() 成功，
        // 则调度一个 GC worker 并将其状态从 _Gwaiting 设置为 _Grunnable，使其处于可运行状态。
        node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
        if node != nil {
            pp.gcMarkWorkerMode = gcMarkWorkerIdleMode
            gp := node.gp.ptr()
            casgstatus(gp, _Gwaiting, _Grunnable)
            if trace.enabled {
                traceGoUnpark(gp, 0)
            }
            return gp, false, false
        }
        gcController.removeIdleMarkWorker()
    }

    // wasm only:
    // If a callback returned and no other goroutine is awake,
    // then wake event handler goroutine which pauses execution
    // until a callback was triggered.
    gp, otherReady := beforeIdle(now, pollUntil)
    if gp != nil {
        casgstatus(gp, _Gwaiting, _Grunnable)
        if trace.enabled {
            traceGoUnpark(gp, 0)
        }
        return gp, false, false
    }
    if otherReady {
        goto top
    }

    // Before we drop our P, make a snapshot of the allp slice,
    // which can change underfoot once we no longer block
    // safe-points. We don't need to snapshot the contents because
    // everything up to cap(allp) is immutable.
    allpSnapshot := allp
    // Also snapshot masks. Value changes are OK, but we can't allow
    // len to change out from under us.
    idlepMaskSnapshot := idlepMask
    timerpMaskSnapshot := timerpMask

    // return P and block
    lock(&sched.lock)
    if sched.gcwaiting.Load() || pp.runSafePointFn != 0 {
        unlock(&sched.lock)
        goto top
    }
    if sched.runqsize != 0 {
        gp := globrunqget(pp, 0)
        unlock(&sched.lock)
        return gp, false, false
    }
    if !mp.spinning && sched.needspinning.Load() == 1 {
        // See "Delicate dance" comment below.
        mp.becomeSpinning()
        unlock(&sched.lock)
        goto top
    }
    // 如果没有可以运行的 goroutine，且 GC 工作也完成，M 会进入空闲状态
    if releasep() != pp {
        throw("findrunnable: wrong p")
    }
    now = pidleput(pp, now)
    unlock(&sched.lock)

    // Delicate dance: thread transitions from spinning to non-spinning
    // state, potentially concurrently with submission of new work. We must
    // drop nmspinning first and then check all sources again (with
    // #StoreLoad memory barrier in between). If we do it the other way
    // around, another thread can submit work after we've checked all
    // sources but before we drop nmspinning; as a result nobody will
    // unpark a thread to run the work.
    //
    // This applies to the following sources of work:
    //
    // * Goroutines added to a per-P run queue.
    // * New/modified-earlier timers on a per-P timer heap.
    // * Idle-priority GC work (barring golang.org/issue/19112).
    //
    // If we discover new work below, we need to restore m.spinning as a
    // signal for resetspinning to unpark a new worker thread (because
    // there can be more than one starving goroutine).
    //
    // However, if after discovering new work we also observe no idle Ps
    // (either here or in resetspinning), we have a problem. We may be
    // racing with a non-spinning M in the block above, having found no
    // work and preparing to release its P and park. Allowing that P to go
    // idle will result in loss of work conservation (idle P while there is
    // runnable work). This could result in complete deadlock in the
    // unlikely event that we discover new work (from netpoll) right as we
    // are racing with _all_ other Ps going idle.
    //
    // We use sched.needspinning to synchronize with non-spinning Ms going
    // idle. If needspinning is set when they are about to drop their P,
    // they abort the drop and instead become a new spinning M on our
    // behalf. If we are not racing and the system is truly fully loaded
    // then no spinning threads are required, and the next thread to
    // naturally become spinning will clear the flag.
    //
    // Also see "Worker thread parking/unparking" comment at the top of the
    // file.
    // 处理 Spinning 转换为非 Spinning 的精细操作
    wasSpinning := mp.spinning
    if mp.spinning {
        mp.spinning = false
        if sched.nmspinning.Add(-1) < 0 {
            throw("findrunnable: negative nmspinning")
        }

        // Note the for correctness, only the last M transitioning from
        // spinning to non-spinning must perform these rechecks to
        // ensure no missed work. However, the runtime has some cases
        // of transient increments of nmspinning that are decremented
        // without going through this path, so we must be conservative
        // and perform the check on all spinning Ms.
        //
        // See https://go.dev/issue/43997.

        // Check all runqueues once again.
        // 再次检查所有运行队列，确保没有错过工作
        pp := checkRunqsNoP(allpSnapshot, idlepMaskSnapshot)
        if pp != nil {
            acquirep(pp)
            mp.becomeSpinning()
            goto top
        }

        // Check for idle-priority GC work again.
        // 再次检查空闲优先级 GC 工作
        pp, gp := checkIdleGCNoP()
        if pp != nil {
            acquirep(pp)
            mp.becomeSpinning()

            // Run the idle worker.
            pp.gcMarkWorkerMode = gcMarkWorkerIdleMode
            casgstatus(gp, _Gwaiting, _Grunnable)
            if trace.enabled {
                traceGoUnpark(gp, 0)
            }
            return gp, false, false
        }

        // Finally, check for timer creation or expiry concurrently with
        // transitioning from spinning to non-spinning.
        //
        // Note that we cannot use checkTimers here because it calls
        // adjusttimers which may need to allocate memory, and that isn't
        // allowed when we don't have an active P.
        // 检查定时器创建或到期
        pollUntil = checkTimersNoP(allpSnapshot, timerpMaskSnapshot, pollUntil)
    }

    // Poll network until next timer.
    // 条件判断是否进行网络轮询
    if netpollinited() && (netpollWaiters.Load() > 0 || pollUntil != 0) && sched.lastpoll.Swap(0) != 0 {
        sched.pollUntil.Store(pollUntil)
        // 可能有网络事件可处理，因此进入网络轮询阶段
        // 轮询网络事件
        // 确保 M 没有绑定 P。网络轮询时，M 不能占用 P，因为轮询过程中 M 会阻塞等待网络事件，而 P 需要继续处理其他 goroutine。
        if mp.p != 0 {
            throw("findrunnable: netpoll with p")
        }
        // M 处于旋转状态时不允许进入网络轮询。如果 M 正在旋转查找工作，则不应进入网络轮询以避免阻塞。
        if mp.spinning {
            throw("findrunnable: netpoll with spinning")
        }
        // Refresh now.
        // 计算网络轮询的超时时间 delay。如果有定时器事件发生，则设置 delay 使轮询等待的时间不超过定时器事件的时间。
        now = nanotime()
        delay := int64(-1)
        if pollUntil != 0 {
            delay = pollUntil - now
            if delay < 0 {
                delay = 0
            }
        }
        if faketime != 0 {
            // When using fake time, just poll.
            delay = 0
        }
        // 执行非阻塞网络轮询
        // 执行网络轮询，阻塞直到有新的网络事件或超时。
        list := netpoll(delay) // block until new work is available
        // 轮询完成后，将 sched.pollUntil 和 sched.lastpoll 重置，确保网络轮询的状态已更新。
        sched.pollUntil.Store(0)
        sched.lastpoll.Store(now)
        if faketime != 0 && list.empty() {
            // Using fake time and nothing is ready; stop M.
            // When all M's stop, checkdead will call timejump.
            // 	如果使用的是虚拟时间 (faketime != 0) 并且没有网络事件，
            // 	当前 M 会停止 (stopm())，释放系统资源，进入空闲状态。
            stopm()
            goto top
        }
        // 分配可运行的 goroutine
        lock(&sched.lock)
        // 检查是否有空闲的P可用
        pp, _ := pidleget(now)
        unlock(&sched.lock)
        // 如果没有空闲的 P，则将网络轮询结果的 goroutine 列表 (list) 注入全局队列。
        if pp == nil {
            injectglist(&list)
        } else {
            // 如果有空闲的 P，则获取该 P 并从网络轮询的结果中选择一个 goroutine 运行，
            // 更新其状态为可运行 (_Grunnable) 并将其返回
            acquirep(pp)
            if !list.empty() {
                gp := list.pop()
                injectglist(&list)
                casgstatus(gp, _Gwaiting, _Grunnable)
                if trace.enabled {
                    traceGoUnpark(gp, 0)
                }
                return gp, false, false
            }
            if wasSpinning {
                mp.becomeSpinning()
            }
            goto top
        }
    } else if pollUntil != 0 && netpollinited() {
        // 继续网络轮询
        // 如果 pollUntil 不为零且网络轮询已初始化，调度器将继续轮询以获取更多的网络事件，直到下一次定时器事件发生。
        pollerPollUntil := sched.pollUntil.Load()
        if pollerPollUntil == 0 || pollerPollUntil > pollUntil {
            netpollBreak()
        }
    }
    // 如果找不到可运行的 goroutine 或网络事件，M 将调用 stopm() 进入空闲状态，等待调度器重新唤醒。
    stopm()
    goto top
}

// pollWork reports whether there is non-background work this P could
// be doing. This is a fairly lightweight check to be used for
// background work loops, like idle GC. It checks a subset of the
// conditions checked by the actual scheduler.
func pollWork() bool {
    if sched.runqsize != 0 {
        return true
    }
    p := getg().m.p.ptr()
    if !runqempty(p) {
        return true
    }
    if netpollinited() && netpollWaiters.Load() > 0 && sched.lastpoll.Load() != 0 {
        if list := netpoll(0); !list.empty() {
            injectglist(&list)
            return true
        }
    }
    return false
}

// stealWork attempts to steal a runnable goroutine or timer from any P.
//
// If newWork is true, new work may have been readied.
//
// If now is not 0 it is the current time. stealWork returns the passed time or
// the current time if now was passed as 0.
func stealWork(now int64) (gp *g, inheritTime bool, rnow, pollUntil int64, newWork bool) {
    pp := getg().m.p.ptr()

    // 标记是否运行了定时器
    ranTimer := false

    // 最多偷取四次
    const stealTries = 4
    for i := 0; i < stealTries; i++ {
        // 最后一次尝试窃取时，优先尝试窃取定时器
        // 只有在最后一次尝试时，才会优先尝试窃取定时器或者 runnext，即最高优先级的 goroutine。
        stealTimersOrRunNextG := i == stealTries-1

        // 遍历P
        // 随机顺序遍历所有的P
        for enum := stealOrder.start(fastrand()); !enum.done(); enum.next() {
            // 如果调度器正在等待垃圾回收 (GC) 进行，则返回，表示可能有 GC 任务。
            if sched.gcwaiting.Load() {
                // GC work may be available.
                return nil, false, now, pollUntil, true
            }
            // 当前遍历的P
            p2 := allp[enum.position()]
            // 是自己则跳过
            if pp == p2 {
                continue
            }

            // Steal timers from p2. This call to checkTimers is the only place
            // where we might hold a lock on a different P's timers. We do this
            // once on the last pass before checking runnext because stealing
            // from the other P's runnext should be the last resort, so if there
            // are timers to steal do that first.
            //
            // We only check timers on one of the stealing iterations because
            // the time stored in now doesn't change in this loop and checking
            // the timers for each P more than once with the same value of now
            // is probably a waste of time.
            //
            // timerpMask tells us whether the P may have timers at all. If it
            // can't, no need to check at all.
            // 偷取定时器任务
            if stealTimersOrRunNextG && timerpMask.read(enum.position()) {
                // 检查 p2 是否有定时器到期任务，如果有就运行。
                tnow, w, ran := checkTimers(p2, now)
                now = tnow
                // 更新定时器的等待时间。
                if w != 0 && (pollUntil == 0 || w < pollUntil) {
                    pollUntil = w
                }
                if ran {
                    // 如果成功运行了定时器任务 (ran 为 true)，
                    // 检查当前 P (pp) 的运行队列 (runqget) 是否有任务可执行。如果有，返回该 goroutine。
                    // Running the timers may have
                    // made an arbitrary number of G's
                    // ready and added them to this P's
                    // local run queue. That invalidates
                    // the assumption of runqsteal
                    // that it always has room to add
                    // stolen G's. So check now if there
                    // is a local G to run.
                    if gp, inheritTime := runqget(pp); gp != nil {
                        return gp, inheritTime, now, pollUntil, ranTimer
                    }
                    ranTimer = true
                }
            }

            // Don't bother to attempt to steal if p2 is idle.
            // 检查 p2 是否处于空闲状态 (idlepMask.read)，如果不空闲，尝试从 p2 中偷取任务 (runqsteal)。
            if !idlepMask.read(enum.position()) {
                if gp := runqsteal(pp, p2, stealTimersOrRunNextG); gp != nil {
                    return gp, false, now, pollUntil, ranTimer
                }
            }
        }
    }

    // No goroutines found to steal. Regardless, running a timer may have
    // made some goroutine ready that we missed. Indicate the next timer to
    // wait for.
    // 如果所有尝试都失败，没有找到可运行的 goroutine 或定时器任务，则返回 nil，
    // 表示当前没有任务可执行，可能需要继续等待其他任务的到来。
    return nil, false, now, pollUntil, ranTimer
}

// Check all Ps for a runnable G to steal.
//
// On entry we have no P. If a G is available to steal and a P is available,
// the P is returned which the caller should acquire and attempt to steal the
// work to.
func checkRunqsNoP(allpSnapshot []*p, idlepMaskSnapshot pMask) *p {
    for id, p2 := range allpSnapshot {
        if !idlepMaskSnapshot.read(uint32(id)) && !runqempty(p2) {
            lock(&sched.lock)
            pp, _ := pidlegetSpinning(0)
            if pp == nil {
                // Can't get a P, don't bother checking remaining Ps.
                unlock(&sched.lock)
                return nil
            }
            unlock(&sched.lock)
            return pp
        }
    }

    // No work available.
    return nil
}

// Check all Ps for a timer expiring sooner than pollUntil.
//
// Returns updated pollUntil value.
func checkTimersNoP(allpSnapshot []*p, timerpMaskSnapshot pMask, pollUntil int64) int64 {
    for id, p2 := range allpSnapshot {
        if timerpMaskSnapshot.read(uint32(id)) {
            w := nobarrierWakeTime(p2)
            if w != 0 && (pollUntil == 0 || w < pollUntil) {
                pollUntil = w
            }
        }
    }

    return pollUntil
}

// Check for idle-priority GC, without a P on entry.
//
// If some GC work, a P, and a worker G are all available, the P and G will be
// returned. The returned P has not been wired yet.
func checkIdleGCNoP() (*p, *g) {
    // N.B. Since we have no P, gcBlackenEnabled may change at any time; we
    // must check again after acquiring a P. As an optimization, we also check
    // if an idle mark worker is needed at all. This is OK here, because if we
    // observe that one isn't needed, at least one is currently running. Even if
    // it stops running, its own journey into the scheduler should schedule it
    // again, if need be (at which point, this check will pass, if relevant).
    if atomic.Load(&gcBlackenEnabled) == 0 || !gcController.needIdleMarkWorker() {
        return nil, nil
    }
    if !gcMarkWorkAvailable(nil) {
        return nil, nil
    }

    // Work is available; we can start an idle GC worker only if there is
    // an available P and available worker G.
    //
    // We can attempt to acquire these in either order, though both have
    // synchronization concerns (see below). Workers are almost always
    // available (see comment in findRunnableGCWorker for the one case
    // there may be none). Since we're slightly less likely to find a P,
    // check for that first.
    //
    // Synchronization: note that we must hold sched.lock until we are
    // committed to keeping it. Otherwise we cannot put the unnecessary P
    // back in sched.pidle without performing the full set of idle
    // transition checks.
    //
    // If we were to check gcBgMarkWorkerPool first, we must somehow handle
    // the assumption in gcControllerState.findRunnableGCWorker that an
    // empty gcBgMarkWorkerPool is only possible if gcMarkDone is running.
    lock(&sched.lock)
    pp, now := pidlegetSpinning(0)
    if pp == nil {
        unlock(&sched.lock)
        return nil, nil
    }

    // Now that we own a P, gcBlackenEnabled can't change (as it requires STW).
    if gcBlackenEnabled == 0 || !gcController.addIdleMarkWorker() {
        pidleput(pp, now)
        unlock(&sched.lock)
        return nil, nil
    }

    node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
    if node == nil {
        pidleput(pp, now)
        unlock(&sched.lock)
        gcController.removeIdleMarkWorker()
        return nil, nil
    }

    unlock(&sched.lock)

    return pp, node.gp.ptr()
}

// wakeNetPoller wakes up the thread sleeping in the network poller if it isn't
// going to wake up before the when argument; or it wakes an idle P to service
// timers and the network poller if there isn't one already.
func wakeNetPoller(when int64) {
    if sched.lastpoll.Load() == 0 {
        // In findrunnable we ensure that when polling the pollUntil
        // field is either zero or the time to which the current
        // poll is expected to run. This can have a spurious wakeup
        // but should never miss a wakeup.
        pollerPollUntil := sched.pollUntil.Load()
        if pollerPollUntil == 0 || pollerPollUntil > when {
            netpollBreak()
        }
    } else {
        // There are no threads in the network poller, try to get
        // one there so it can handle new timers.
        if GOOS != "plan9" { // Temporary workaround - see issue #42303.
            wakep()
        }
    }
}

func resetspinning() {
    gp := getg()
    if !gp.m.spinning {
        throw("resetspinning: not a spinning m")
    }
    // 停止当前M的自旋状态
    gp.m.spinning = false
    // 更新M的全局计数，确保旋转状态计数正确
    nmspinning := sched.nmspinning.Add(-1)
    if nmspinning < 0 {
        throw("findrunnable: negative nmspinning")
    }
    // M wakeup policy is deliberately somewhat conservative, so check if we
    // need to wakeup another P here. See "Worker thread parking/unparking"
    // comment at the top of the file for details.
    // 检查是否需要唤醒其他 M 来处理新的任务，保持系统的运行效率。
    wakep()
}

// injectglist adds each runnable G on the list to some run queue,
// and clears glist. If there is no current P, they are added to the
// global queue, and up to npidle M's are started to run them.
// Otherwise, for each idle P, this adds a G to the global queue
// and starts an M. Any remaining G's are added to the current P's
// local run queue.
// This may temporarily acquire sched.lock.
// Can run concurrently with GC.
func injectglist(glist *gList) {
    if glist.empty() {
        return
    }
    if trace.enabled {
        for gp := glist.head.ptr(); gp != nil; gp = gp.schedlink.ptr() {
            traceGoUnpark(gp, 0)
        }
    }

    // Mark all the goroutines as runnable before we put them
    // on the run queues.
    head := glist.head.ptr()
    var tail *g
    qsize := 0
    for gp := head; gp != nil; gp = gp.schedlink.ptr() {
        tail = gp
        qsize++
        casgstatus(gp, _Gwaiting, _Grunnable)
    }

    // Turn the gList into a gQueue.
    var q gQueue
    q.head.set(head)
    q.tail.set(tail)
    *glist = gList{}

    startIdle := func(n int) {
        for i := 0; i < n; i++ {
            mp := acquirem() // See comment in startm.
            lock(&sched.lock)

            pp, _ := pidlegetSpinning(0)
            if pp == nil {
                unlock(&sched.lock)
                releasem(mp)
                break
            }

            startm(pp, false, true)
            unlock(&sched.lock)
            releasem(mp)
        }
    }

    pp := getg().m.p.ptr()
    if pp == nil {
        lock(&sched.lock)
        globrunqputbatch(&q, int32(qsize))
        unlock(&sched.lock)
        startIdle(qsize)
        return
    }

    npidle := int(sched.npidle.Load())
    var globq gQueue
    var n int
    for n = 0; n < npidle && !q.empty(); n++ {
        g := q.pop()
        globq.pushBack(g)
    }
    if n > 0 {
        lock(&sched.lock)
        globrunqputbatch(&globq, int32(n))
        unlock(&sched.lock)
        startIdle(n)
        qsize -= n
    }

    if !q.empty() {
        runqputbatch(pp, &q, qsize)
    }
}

// One round of scheduler: find a runnable goroutine and execute it.
// Never returns.
func schedule() {
    mp := getg().m

    // 检查线程的锁定状态
    // 调度器不能在 M（线程）持有锁的情况下进行。
    // mp.locks 表示当前线程是否持有任何锁。
    // 如果锁没有释放就进入调度器，会导致死锁，因此直接抛出异常。
    if mp.locks != 0 {
        throw("schedule: holding locks")
    }

    // m.lockedg 会在 LockOSThread 下变为非零
    // LockOSThread 是一种将 goroutine 绑定到特定系统线程的机制。
    // 这里检查 mp.lockedg 是否非零，如果非零，说明当前线程有一个绑定的 goroutine，应该优先调度它。
    if mp.lockedg != 0 {
        stoplockedm()
        execute(mp.lockedg.ptr(), false) // Never returns.
    }

    // We should not schedule away from a g that is executing a cgo call,
    // since the cgo call is using the m's g0 stack.
    // 我们不应该调度远离正在执行 cgo 调用的 g，因为 cgo 调用正在使用 m 的 g0 堆栈。
    // cgo 调用依赖于 M 的 g0 栈，因此如果当前线程正在执行 cgo 调用，调度器不能切换到其他 goroutine。
    if mp.incgo {
        throw("schedule: in cgo")
    }

top:
    pp := mp.p.ptr()
    // 当前不允许抢占调度
    pp.preempt = false

    // Safety check: if we are spinning, the run queue should be empty.
    // Check this before calling checkTimers, as that might call
    // goready to put a ready goroutine on the local run queue.
    // 安全检查：如果我们正在旋转，则运行队列应该为空。
    // 在调用 checkTimers 之前检查此项，因为这可能会调用 goready 将就绪的 Goroutine 放入本地运行队列。
    if mp.spinning && (pp.runnext != 0 || pp.runqhead != pp.runqtail) {
        throw("schedule: spinning with local work")
    }

    // 该函数返回可运行的 goroutine gp、是否继承调度时间 inheritTime 以及是否需要唤醒其他 P (tryWakeP)。
    gp, inheritTime, tryWakeP := findRunnable() // blocks until work is available

    // This thread is going to run a goroutine and is not spinning anymore,
    // so if it was marked as spinning we need to reset it now and potentially
    // start a new spinning M.
    // 如果当前线程在执行前是处于旋转状态，则需要将其标记为不再旋转，并重置旋转状态。
    if mp.spinning {
        resetspinning()
    }

    // 处理调度禁用
    if sched.disable.user && !schedEnabled(gp) {
        // Scheduling of this goroutine is disabled. Put it on
        // the list of pending runnable goroutines for when we
        // re-enable user scheduling and look again.
        // 调度器支持禁用某些 goroutine 的调度。
        // 如果调度被禁用且当前 goroutine 不允许调度，则将它放到等待运行的列表中。
        lock(&sched.lock)
        if schedEnabled(gp) {
            // Something re-enabled scheduling while we
            // were acquiring the lock.
            unlock(&sched.lock)
        } else {
            sched.disable.runnable.pushBack(gp)
            sched.disable.n++
            unlock(&sched.lock)
            goto top
        }
    }

    // If about to schedule a not-normal goroutine (a GCworker or tracereader),
    // wake a P if there is one.
    // 如果返回的 goroutine 不是一个 “正常” 的 goroutine（例如 GC worker 或 trace reader），调度器需要唤醒一个 P 来处理这些特殊任务。
    if tryWakeP {
        wakep()
    }
    // 检查 goroutine 是否锁定在某个线程
    if gp.lockedm != 0 {
        // Hands off own p to the locked m,
        // then blocks waiting for a new p.
        // 如果 goroutine 锁定了某个特定的 M，调度器需要将当前的 P 交给该 M 并等待新的 P。
        startlockedm(gp)
        goto top
    }

    // 执行 Goroutine
    execute(gp, inheritTime)
}

// dropg removes the association between m and the current goroutine m->curg (gp for short).
// Typically a caller sets gp's status away from Grunning and then
// immediately calls dropg to finish the job. The caller is also responsible
// for arranging that gp will be restarted using ready at an
// appropriate time. After calling dropg and arranging for gp to be
// readied later, the caller can do other work but eventually should
// call schedule to restart the scheduling of goroutines on this m.
// dropg 删除 m 与当前 Goroutine m->curg（简称 gp）之间的关联。
// 通常，调用者将 gp 的状态设置为远离 Grunning，然后立即调用 dropg 来完成工作。
// 调用者还负责安排gp 在适当的时间使用ready 重新启动。
// 在调用 dropg 并安排 gp 稍后准备好之后，调用者可以做其他工作，但最终应该调用 Schedule 来重新启动此 m 上的 goroutine 的调度。
func dropg() {
    gp := getg()

    setMNoWB(&gp.m.curg.m, nil)
    setGNoWB(&gp.m.curg, nil)
}

// checkTimers runs any timers for the P that are ready.
// If now is not 0 it is the current time.
// It returns the passed time or the current time if now was passed as 0.
// and the time when the next timer should run or 0 if there is no next timer,
// and reports whether it ran any timers.
// If the time when the next timer should run is not 0,
// it is always larger than the returned time.
// We pass now in and out to avoid extra calls of nanotime.
//
//go:yeswritebarrierrec
func checkTimers(pp *p, now int64) (rnow, pollUntil int64, ran bool) {
    // If it's not yet time for the first timer, or the first adjusted
    // timer, then there is nothing to do.
    next := pp.timer0When.Load()
    nextAdj := pp.timerModifiedEarliest.Load()
    if next == 0 || (nextAdj != 0 && nextAdj < next) {
        next = nextAdj
    }

    if next == 0 {
        // No timers to run or adjust.
        return now, 0, false
    }

    if now == 0 {
        now = nanotime()
    }
    if now < next {
        // Next timer is not ready to run, but keep going
        // if we would clear deleted timers.
        // This corresponds to the condition below where
        // we decide whether to call clearDeletedTimers.
        if pp != getg().m.p.ptr() || int(pp.deletedTimers.Load()) <= int(pp.numTimers.Load()/4) {
            return now, next, false
        }
    }

    lock(&pp.timersLock)

    if len(pp.timers) > 0 {
        adjusttimers(pp, now)
        for len(pp.timers) > 0 {
            // Note that runtimer may temporarily unlock
            // pp.timersLock.
            if tw := runtimer(pp, now); tw != 0 {
                if tw > 0 {
                    pollUntil = tw
                }
                break
            }
            ran = true
        }
    }

    // If this is the local P, and there are a lot of deleted timers,
    // clear them out. We only do this for the local P to reduce
    // lock contention on timersLock.
    if pp == getg().m.p.ptr() && int(pp.deletedTimers.Load()) > len(pp.timers)/4 {
        clearDeletedTimers(pp)
    }

    unlock(&pp.timersLock)

    return now, pollUntil, ran
}

func parkunlock_c(gp *g, lock unsafe.Pointer) bool {
    unlock((*mutex)(lock))
    return true
}

// park continuation on g0.
func park_m(gp *g) {
    mp := getg().m

    if trace.enabled {
        traceGoPark(mp.waittraceev, mp.waittraceskip)
    }

    // N.B. Not using casGToWaiting here because the waitreason is
    // set by park_m's caller.
    casgstatus(gp, _Grunning, _Gwaiting)
    dropg()

    if fn := mp.waitunlockf; fn != nil {
        ok := fn(gp, mp.waitlock)
        mp.waitunlockf = nil
        mp.waitlock = nil
        if !ok {
            if trace.enabled {
                traceGoUnpark(gp, 2)
            }
            casgstatus(gp, _Gwaiting, _Grunnable)
            execute(gp, true) // Schedule it back, never returns.
        }
    }
    schedule()
}

func goschedImpl(gp *g) {
    status := readgstatus(gp)
    if status&^_Gscan != _Grunning {
        dumpgstatus(gp)
        throw("bad g status")
    }
    casgstatus(gp, _Grunning, _Grunnable)
    dropg()
    lock(&sched.lock)
    globrunqput(gp)
    unlock(&sched.lock)

    schedule()
}

// Gosched continuation on g0.
func gosched_m(gp *g) {
    if trace.enabled {
        traceGoSched()
    }
    goschedImpl(gp)
}

// goschedguarded is a forbidden-states-avoided version of gosched_m.
func goschedguarded_m(gp *g) {

    if !canPreemptM(gp.m) {
        gogo(&gp.sched) // never return
    }

    if trace.enabled {
        traceGoSched()
    }
    goschedImpl(gp)
}

func gopreempt_m(gp *g) {
    if trace.enabled {
        traceGoPreempt()
    }
    goschedImpl(gp)
}

// preemptPark parks gp and puts it in _Gpreempted.
//
//go:systemstack
func preemptPark(gp *g) {
    if trace.enabled {
        traceGoPark(traceEvGoBlock, 0)
    }
    status := readgstatus(gp)
    if status&^_Gscan != _Grunning {
        dumpgstatus(gp)
        throw("bad g status")
    }

    if gp.asyncSafePoint {
        // Double-check that async preemption does not
        // happen in SPWRITE assembly functions.
        // isAsyncSafePoint must exclude this case.
        f := findfunc(gp.sched.pc)
        if !f.valid() {
            throw("preempt at unknown pc")
        }
        if f.flag&funcFlag_SPWRITE != 0 {
            println("runtime: unexpected SPWRITE function", funcname(f), "in async preempt")
            throw("preempt SPWRITE")
        }
    }

    // Transition from _Grunning to _Gscan|_Gpreempted. We can't
    // be in _Grunning when we dropg because then we'd be running
    // without an M, but the moment we're in _Gpreempted,
    // something could claim this G before we've fully cleaned it
    // up. Hence, we set the scan bit to lock down further
    // transitions until we can dropg.
    casGToPreemptScan(gp, _Grunning, _Gscan|_Gpreempted)
    dropg()
    casfrom_Gscanstatus(gp, _Gscan|_Gpreempted, _Gpreempted)
    schedule()
}

// goyield is like Gosched, but it:
// - emits a GoPreempt trace event instead of a GoSched trace event
// - puts the current G on the runq of the current P instead of the globrunq
func goyield() {
    checkTimeouts()
    mcall(goyield_m)
}

func goyield_m(gp *g) {
    if trace.enabled {
        traceGoPreempt()
    }
    pp := gp.m.p.ptr()
    casgstatus(gp, _Grunning, _Grunnable)
    dropg()
    runqput(pp, gp, false)
    schedule()
}

// Finishes execution of the current goroutine.
func goexit1() {
    if raceenabled {
        racegoend()
    }
    if trace.enabled {
        traceGoEnd()
    }
    mcall(goexit0)
}

// goexit continuation on g0.
// g0上执行goexit
func goexit0(gp *g) {
    mp := getg().m
    pp := mp.p.ptr()

    // 切换当前的 g 为 _Gdead
    casgstatus(gp, _Grunning, _Gdead)
    gcController.addScannableStack(pp, -int64(gp.stack.hi-gp.stack.lo))
    if isSystemGoroutine(gp, false) {
        sched.ngsys.Add(-1)
    }
    // 清理
    gp.m = nil
    locked := gp.lockedm != 0
    gp.lockedm = 0
    mp.lockedg = 0
    gp.preemptStop = false
    gp.paniconfault = false
    gp._defer = nil // should be true already but just in case.
    gp._panic = nil // non-nil for Goexit during panic. points at stack-allocated data.
    gp.writebuf = nil
    gp.waitreason = waitReasonZero
    gp.param = nil
    gp.labels = nil
    gp.timer = nil

    if gcBlackenEnabled != 0 && gp.gcAssistBytes > 0 {
        // Flush assist credit to the global pool. This gives
        // better information to pacing if the application is
        // rapidly creating an exiting goroutines.
        assistWorkPerByte := gcController.assistWorkPerByte.Load()
        scanCredit := int64(assistWorkPerByte * float64(gp.gcAssistBytes))
        gcController.bgScanCredit.Add(scanCredit)
        gp.gcAssistBytes = 0
    }

    // 解绑g和m
    dropg()

    // wasm 目前还没有线程支持
    if GOARCH == "wasm" { // no threads yet on wasm
        // 将 g 扔进 gfree 链表中等待复用
        gfput(pp, gp)
        // 再次进行调度
        schedule() // never returns
    }

    if mp.lockedInt != 0 {
        print("invalid m->lockedInt = ", mp.lockedInt, "\n")
        throw("internal lockOSThread error")
    }

    // 将 g 扔进 gfree 链表中等待复用
    gfput(pp, gp)
    if locked {
        // The goroutine may have locked this thread because
        // it put it in an unusual kernel state. Kill it
        // rather than returning it to the thread pool.
        // 协程执行完了，但还是在当前线程锁住，这时需要退出线程
        // 该 Goroutine 可能在当前线程上锁住，因为它可能导致了不正常的内核状态
        // 这时候 kill 该线程，而非将 m 放回到线程池。

        // Return to mstart, which will release the P and exit
        // the thread.
        // 此举会返回到 mstart，从而释放当前的 P 并退出该线程
        if GOOS != "plan9" { // See golang.org/issue/22227.
            gogo(&mp.g0.sched)
        } else {
            // Clear lockedExt on plan9 since we may end up re-using
            // this thread.
            // // 因为我们可能已重用此线程结束，在 plan9 上清除 lockedExt
            mp.lockedExt = 0
        }
    }
    // 再次进行调度
    schedule()
}

// save updates getg().sched to refer to pc and sp so that a following
// gogo will restore pc and sp.
//
// save must not have write barriers because invoking a write barrier
// can clobber getg().sched.
//
//go:nosplit
//go:nowritebarrierrec
func save(pc, sp uintptr) {
    gp := getg()

    if gp == gp.m.g0 || gp == gp.m.gsignal {
        // m.g0.sched is special and must describe the context
        // for exiting the thread. mstart1 writes to it directly.
        // m.gsignal.sched should not be used at all.
        // This check makes sure save calls do not accidentally
        // run in contexts where they'd write to system g's.
        throw("save on system g not allowed")
    }

    gp.sched.pc = pc
    gp.sched.sp = sp
    gp.sched.lr = 0
    gp.sched.ret = 0
    // We need to ensure ctxt is zero, but can't have a write
    // barrier here. However, it should always already be zero.
    // Assert that.
    if gp.sched.ctxt != nil {
        badctxt()
    }
}

// The goroutine g is about to enter a system call.
// Record that it's not using the cpu anymore.
// This is called only from the go syscall library and cgocall,
// not from the low-level system calls used by the runtime.
//
// Entersyscall cannot split the stack: the save must
// make g->sched refer to the caller's stack segment, because
// entersyscall is going to return immediately after.
//
// Nothing entersyscall calls can split the stack either.
// We cannot safely move the stack during an active call to syscall,
// because we do not know which of the uintptr arguments are
// really pointers (back into the stack).
// In practice, this means that we make the fast path run through
// entersyscall doing no-split things, and the slow path has to use systemstack
// to run bigger things on the system stack.
//
// reentersyscall is the entry point used by cgo callbacks, where explicitly
// saved SP and PC are restored. This is needed when exitsyscall will be called
// from a function further up in the call stack than the parent, as g->syscallsp
// must always point to a valid stack frame. entersyscall below is the normal
// entry point for syscalls, which obtains the SP and PC from the caller.
//
// Syscall tracing:
// At the start of a syscall we emit traceGoSysCall to capture the stack trace.
// If the syscall does not block, that is it, we do not emit any other events.
// If the syscall blocks (that is, P is retaken), retaker emits traceGoSysBlock;
// when syscall returns we emit traceGoSysExit and when the goroutine starts running
// (potentially instantly, if exitsyscallfast returns true) we emit traceGoStart.
// To ensure that traceGoSysExit is emitted strictly after traceGoSysBlock,
// we remember current value of syscalltick in m (gp.m.syscalltick = gp.m.p.ptr().syscalltick),
// whoever emits traceGoSysBlock increments p.syscalltick afterwards;
// and we wait for the increment before emitting traceGoSysExit.
// Note that the increment is done even if tracing is not enabled,
// because tracing can be enabled in the middle of syscall. We don't want the wait to hang.
//
//go:nosplit
func reentersyscall(pc, sp uintptr) {
    gp := getg()

    // Disable preemption because during this function g is in Gsyscall status,
    // but can have inconsistent g->sched, do not let GC observe it.
    gp.m.locks++

    // Entersyscall must not call any function that might split/grow the stack.
    // (See details in comment above.)
    // Catch calls that might, by replacing the stack guard with something that
    // will trip any stack check and leaving a flag to tell newstack to die.
    gp.stackguard0 = stackPreempt
    gp.throwsplit = true

    // Leave SP around for GC and traceback.
    save(pc, sp)
    gp.syscallsp = sp
    gp.syscallpc = pc
    casgstatus(gp, _Grunning, _Gsyscall)
    if gp.syscallsp < gp.stack.lo || gp.stack.hi < gp.syscallsp {
        systemstack(func() {
            print("entersyscall inconsistent ", hex(gp.syscallsp), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
            throw("entersyscall")
        })
    }

    if trace.enabled {
        systemstack(traceGoSysCall)
        // systemstack itself clobbers g.sched.{pc,sp} and we might
        // need them later when the G is genuinely blocked in a
        // syscall
        save(pc, sp)
    }

    if sched.sysmonwait.Load() {
        systemstack(entersyscall_sysmon)
        save(pc, sp)
    }

    if gp.m.p.ptr().runSafePointFn != 0 {
        // runSafePointFn may stack split if run on this stack
        systemstack(runSafePointFn)
        save(pc, sp)
    }

    gp.m.syscalltick = gp.m.p.ptr().syscalltick
    gp.sysblocktraced = true
    pp := gp.m.p.ptr()
    pp.m = 0
    gp.m.oldp.set(pp)
    gp.m.p = 0
    atomic.Store(&pp.status, _Psyscall)
    if sched.gcwaiting.Load() {
        systemstack(entersyscall_gcwait)
        save(pc, sp)
    }

    gp.m.locks--
}

// Standard syscall entry used by the go syscall library and normal cgo calls.
//
// This is exported via linkname to assembly in the syscall package and x/sys.
//
//go:nosplit
//go:linkname entersyscall
func entersyscall() {
    reentersyscall(getcallerpc(), getcallersp())
}

func entersyscall_sysmon() {
    lock(&sched.lock)
    if sched.sysmonwait.Load() {
        sched.sysmonwait.Store(false)
        notewakeup(&sched.sysmonnote)
    }
    unlock(&sched.lock)
}

func entersyscall_gcwait() {
    gp := getg()
    pp := gp.m.oldp.ptr()

    lock(&sched.lock)
    if sched.stopwait > 0 && atomic.Cas(&pp.status, _Psyscall, _Pgcstop) {
        if trace.enabled {
            traceGoSysBlock(pp)
            traceProcStop(pp)
        }
        pp.syscalltick++
        if sched.stopwait--; sched.stopwait == 0 {
            notewakeup(&sched.stopnote)
        }
    }
    unlock(&sched.lock)
}

// The same as entersyscall(), but with a hint that the syscall is blocking.
//
//go:nosplit
func entersyscallblock() {
    gp := getg()

    gp.m.locks++ // see comment in entersyscall
    gp.throwsplit = true
    gp.stackguard0 = stackPreempt // see comment in entersyscall
    gp.m.syscalltick = gp.m.p.ptr().syscalltick
    gp.sysblocktraced = true
    gp.m.p.ptr().syscalltick++

    // Leave SP around for GC and traceback.
    pc := getcallerpc()
    sp := getcallersp()
    save(pc, sp)
    gp.syscallsp = gp.sched.sp
    gp.syscallpc = gp.sched.pc
    if gp.syscallsp < gp.stack.lo || gp.stack.hi < gp.syscallsp {
        sp1 := sp
        sp2 := gp.sched.sp
        sp3 := gp.syscallsp
        systemstack(func() {
            print("entersyscallblock inconsistent ", hex(sp1), " ", hex(sp2), " ", hex(sp3), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
            throw("entersyscallblock")
        })
    }
    casgstatus(gp, _Grunning, _Gsyscall)
    if gp.syscallsp < gp.stack.lo || gp.stack.hi < gp.syscallsp {
        systemstack(func() {
            print("entersyscallblock inconsistent ", hex(sp), " ", hex(gp.sched.sp), " ", hex(gp.syscallsp), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
            throw("entersyscallblock")
        })
    }

    systemstack(entersyscallblock_handoff)

    // Resave for traceback during blocked call.
    save(getcallerpc(), getcallersp())

    gp.m.locks--
}

func entersyscallblock_handoff() {
    if trace.enabled {
        traceGoSysCall()
        traceGoSysBlock(getg().m.p.ptr())
    }
    handoffp(releasep())
}

// The goroutine g exited its system call.
// Arrange for it to run on a cpu again.
// This is called only from the go syscall library, not
// from the low-level system calls used by the runtime.
//
// Write barriers are not allowed because our P may have been stolen.
//
// This is exported via linkname to assembly in the syscall package.
//
//go:nosplit
//go:nowritebarrierrec
//go:linkname exitsyscall
func exitsyscall() {
    gp := getg()

    gp.m.locks++ // see comment in entersyscall
    if getcallersp() > gp.syscallsp {
        throw("exitsyscall: syscall frame is no longer valid")
    }

    gp.waitsince = 0
    oldp := gp.m.oldp.ptr()
    gp.m.oldp = 0
    if exitsyscallfast(oldp) {
        // When exitsyscallfast returns success, we have a P so can now use
        // write barriers
        if goroutineProfile.active {
            // Make sure that gp has had its stack written out to the goroutine
            // profile, exactly as it was when the goroutine profiler first
            // stopped the world.
            systemstack(func() {
                tryRecordGoroutineProfileWB(gp)
            })
        }
        if trace.enabled {
            if oldp != gp.m.p.ptr() || gp.m.syscalltick != gp.m.p.ptr().syscalltick {
                systemstack(traceGoStart)
            }
        }
        // There's a cpu for us, so we can run.
        gp.m.p.ptr().syscalltick++
        // We need to cas the status and scan before resuming...
        casgstatus(gp, _Gsyscall, _Grunning)

        // Garbage collector isn't running (since we are),
        // so okay to clear syscallsp.
        gp.syscallsp = 0
        gp.m.locks--
        if gp.preempt {
            // restore the preemption request in case we've cleared it in newstack
            gp.stackguard0 = stackPreempt
        } else {
            // otherwise restore the real _StackGuard, we've spoiled it in entersyscall/entersyscallblock
            gp.stackguard0 = gp.stack.lo + _StackGuard
        }
        gp.throwsplit = false

        if sched.disable.user && !schedEnabled(gp) {
            // Scheduling of this goroutine is disabled.
            Gosched()
        }

        return
    }

    gp.sysexitticks = 0
    if trace.enabled {
        // Wait till traceGoSysBlock event is emitted.
        // This ensures consistency of the trace (the goroutine is started after it is blocked).
        for oldp != nil && oldp.syscalltick == gp.m.syscalltick {
            osyield()
        }
        // We can't trace syscall exit right now because we don't have a P.
        // Tracing code can invoke write barriers that cannot run without a P.
        // So instead we remember the syscall exit time and emit the event
        // in execute when we have a P.
        gp.sysexitticks = cputicks()
    }

    gp.m.locks--

    // Call the scheduler.
    mcall(exitsyscall0)

    // Scheduler returned, so we're allowed to run now.
    // Delete the syscallsp information that we left for
    // the garbage collector during the system call.
    // Must wait until now because until gosched returns
    // we don't know for sure that the garbage collector
    // is not running.
    gp.syscallsp = 0
    gp.m.p.ptr().syscalltick++
    gp.throwsplit = false
}

//go:nosplit
func exitsyscallfast(oldp *p) bool {
    gp := getg()

    // Freezetheworld sets stopwait but does not retake P's.
    if sched.stopwait == freezeStopWait {
        return false
    }

    // Try to re-acquire the last P.
    if oldp != nil && oldp.status == _Psyscall && atomic.Cas(&oldp.status, _Psyscall, _Pidle) {
        // There's a cpu for us, so we can run.
        wirep(oldp)
        exitsyscallfast_reacquired()
        return true
    }

    // Try to get any other idle P.
    if sched.pidle != 0 {
        var ok bool
        systemstack(func() {
            ok = exitsyscallfast_pidle()
            if ok && trace.enabled {
                if oldp != nil {
                    // Wait till traceGoSysBlock event is emitted.
                    // This ensures consistency of the trace (the goroutine is started after it is blocked).
                    for oldp.syscalltick == gp.m.syscalltick {
                        osyield()
                    }
                }
                traceGoSysExit(0)
            }
        })
        if ok {
            return true
        }
    }
    return false
}

// exitsyscallfast_reacquired is the exitsyscall path on which this G
// has successfully reacquired the P it was running on before the
// syscall.
//
//go:nosplit
func exitsyscallfast_reacquired() {
    gp := getg()
    if gp.m.syscalltick != gp.m.p.ptr().syscalltick {
        if trace.enabled {
            // The p was retaken and then enter into syscall again (since gp.m.syscalltick has changed).
            // traceGoSysBlock for this syscall was already emitted,
            // but here we effectively retake the p from the new syscall running on the same p.
            systemstack(func() {
                // Denote blocking of the new syscall.
                traceGoSysBlock(gp.m.p.ptr())
                // Denote completion of the current syscall.
                traceGoSysExit(0)
            })
        }
        gp.m.p.ptr().syscalltick++
    }
}

func exitsyscallfast_pidle() bool {
    lock(&sched.lock)
    pp, _ := pidleget(0)
    if pp != nil && sched.sysmonwait.Load() {
        sched.sysmonwait.Store(false)
        notewakeup(&sched.sysmonnote)
    }
    unlock(&sched.lock)
    if pp != nil {
        acquirep(pp)
        return true
    }
    return false
}

// exitsyscall slow path on g0.
// Failed to acquire P, enqueue gp as runnable.
//
// Called via mcall, so gp is the calling g from this M.
//
//go:nowritebarrierrec
func exitsyscall0(gp *g) {
    casgstatus(gp, _Gsyscall, _Grunnable)
    dropg()
    lock(&sched.lock)
    var pp *p
    if schedEnabled(gp) {
        pp, _ = pidleget(0)
    }
    var locked bool
    if pp == nil {
        globrunqput(gp)

        // Below, we stoplockedm if gp is locked. globrunqput releases
        // ownership of gp, so we must check if gp is locked prior to
        // committing the release by unlocking sched.lock, otherwise we
        // could race with another M transitioning gp from unlocked to
        // locked.
        locked = gp.lockedm != 0
    } else if sched.sysmonwait.Load() {
        sched.sysmonwait.Store(false)
        notewakeup(&sched.sysmonnote)
    }
    unlock(&sched.lock)
    if pp != nil {
        acquirep(pp)
        execute(gp, false) // Never returns.
    }
    if locked {
        // Wait until another thread schedules gp and so m again.
        //
        // N.B. lockedm must be this M, as this g was running on this M
        // before entersyscall.
        stoplockedm()
        execute(gp, false) // Never returns.
    }
    stopm()
    schedule() // Never returns.
}

// Called from syscall package before fork.
//
//go:linkname syscall_runtime_BeforeFork syscall.runtime_BeforeFork
//go:nosplit
func syscall_runtime_BeforeFork() {
    gp := getg().m.curg

    // Block signals during a fork, so that the child does not run
    // a signal handler before exec if a signal is sent to the process
    // group. See issue #18600.
    gp.m.locks++
    sigsave(&gp.m.sigmask)
    sigblock(false)

    // This function is called before fork in syscall package.
    // Code between fork and exec must not allocate memory nor even try to grow stack.
    // Here we spoil g->_StackGuard to reliably detect any attempts to grow stack.
    // runtime_AfterFork will undo this in parent process, but not in child.
    gp.stackguard0 = stackFork
}

// Called from syscall package after fork in parent.
//
//go:linkname syscall_runtime_AfterFork syscall.runtime_AfterFork
//go:nosplit
func syscall_runtime_AfterFork() {
    gp := getg().m.curg

    // See the comments in beforefork.
    gp.stackguard0 = gp.stack.lo + _StackGuard

    msigrestore(gp.m.sigmask)

    gp.m.locks--
}

// inForkedChild is true while manipulating signals in the child process.
// This is used to avoid calling libc functions in case we are using vfork.
var inForkedChild bool

// Called from syscall package after fork in child.
// It resets non-sigignored signals to the default handler, and
// restores the signal mask in preparation for the exec.
//
// Because this might be called during a vfork, and therefore may be
// temporarily sharing address space with the parent process, this must
// not change any global variables or calling into C code that may do so.
//
//go:linkname syscall_runtime_AfterForkInChild syscall.runtime_AfterForkInChild
//go:nosplit
//go:nowritebarrierrec
func syscall_runtime_AfterForkInChild() {
    // It's OK to change the global variable inForkedChild here
    // because we are going to change it back. There is no race here,
    // because if we are sharing address space with the parent process,
    // then the parent process can not be running concurrently.
    inForkedChild = true

    clearSignalHandlers()

    // When we are the child we are the only thread running,
    // so we know that nothing else has changed gp.m.sigmask.
    msigrestore(getg().m.sigmask)

    inForkedChild = false
}

// pendingPreemptSignals is the number of preemption signals
// that have been sent but not received. This is only used on Darwin.
// For #41702.
var pendingPreemptSignals atomic.Int32

// Called from syscall package before Exec.
//
//go:linkname syscall_runtime_BeforeExec syscall.runtime_BeforeExec
func syscall_runtime_BeforeExec() {
    // Prevent thread creation during exec.
    execLock.lock()

    // On Darwin, wait for all pending preemption signals to
    // be received. See issue #41702.
    if GOOS == "darwin" || GOOS == "ios" {
        for pendingPreemptSignals.Load() > 0 {
            osyield()
        }
    }
}

// Called from syscall package after Exec.
//
//go:linkname syscall_runtime_AfterExec syscall.runtime_AfterExec
func syscall_runtime_AfterExec() {
    execLock.unlock()
}

// Allocate a new g, with a stack big enough for stacksize bytes.
// 分配g0栈
func malg(stacksize int32) *g {
    newg := new(g)
    if stacksize >= 0 {
        stacksize = round2(_StackSystem + stacksize)
        systemstack(func() {
            newg.stack = stackalloc(uint32(stacksize))
        })
        newg.stackguard0 = newg.stack.lo + _StackGuard
        newg.stackguard1 = ^uintptr(0)
        // Clear the bottom word of the stack. We record g
        // there on gsignal stack during VDSO on ARM and ARM64.
        *(*uintptr)(unsafe.Pointer(newg.stack.lo)) = 0
    }
    return newg
}

// Create a new g running fn.
// Put it on the queue of g's waiting to run.
// The compiler turns a go statement into a call to this.
// 创建一个新的g去运行方法
func newproc(fn *funcval) {
    // 获取当前正在执行的G
    gp := getg()
    // 获取调用者的程序计数器PC，用于调试和跟踪
    pc := getcallerpc()
    systemstack(func() {
        newg := newproc1(fn, gp, pc)

        // 获取当前的P
        pp := getg().m.p.ptr()
        // 将新创建的协程放入处理器的运行队列
        // next为true表示放在队列尾部
        runqput(pp, newg, true)

        // 如果程序已经启动（main函数已经开始运行）
        // 唤醒正在休眠的处理器来处理新加入的协程
        if mainStarted {
            wakep()
        }
    })
}

// Create a new g in state _Grunnable, starting at fn. callerpc is the
// address of the go statement that created this. The caller is responsible
// for adding the new g to the scheduler.
// 创建一个处于可运行状态的G来运行函数
func newproc1(fn *funcval, callergp *g, callerpc uintptr) *g {
    if fn == nil {
        fatal("go of nil func value")
    }

    mp := acquirem() // disable preemption because we hold M and P in local vars.
    pp := mp.p.ptr()
    // 根据P获取一个空闲的g
    newg := gfget(pp)
    // 初始化阶段，gfget 是不可能找到 g 的
    // 也可能运行中本来就已经耗尽了
    if newg == nil {
        // 创建一个拥有 _StackMin 大小的栈的 g
        newg = malg(_StackMin)
        // 将新创建的 g 从 _Gidle 更新为 _Gdead 状态
        casgstatus(newg, _Gidle, _Gdead)
        // 将 Gdead 状态的 g 添加到 allg，这样 GC 不会扫描未初始化的栈
        allgadd(newg) // publishes with a g->status of Gdead so GC scanner doesn't look at uninitialized stack.
    }
    if newg.stack.hi == 0 {
        throw("newproc1: newg missing stack")
    }

    if readgstatus(newg) != _Gdead {
        throw("newproc1: new g is not Gdead")
    }

    // 计算运行空间的大小
    totalSize := uintptr(4*goarch.PtrSize + sys.MinFrameSize) // extra space in case of reads slightly beyond frame
    totalSize = alignUp(totalSize, sys.StackAlign)
    // 确定 sp 和参数入栈位置
    sp := newg.stack.hi - totalSize
    spArg := sp
    if usesLR {
        // caller's LR
        *(*uintptr)(unsafe.Pointer(sp)) = 0
        prepGoExitFrame(sp)
        spArg += sys.MinFrameSize
    }

    // 清理创建并初始化g的运行现场
    memclrNoHeapPointers(unsafe.Pointer(&newg.sched), unsafe.Sizeof(newg.sched))
    newg.sched.sp = sp
    newg.stktopsp = sp
    newg.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum // +PCQuantum so that previous instruction is in same function
    newg.sched.g = guintptr(unsafe.Pointer(newg))
    // 为之后的协程调度埋下伏笔，执行完该函数，会在栈中保存goexit函数的地址。
    // 协程执行完成返回后，会在栈顶弹出返回地址并加载到程序计数器PC中，并执行goexit
    gostartcallfn(&newg.sched, fn)
    // 初始化g的基本状态
    newg.gopc = callerpc
    newg.ancestors = saveAncestors(callergp) // 调试相关，追踪调用方
    newg.startpc = fn.fn                     // 入口pc
    // 判断是否是系统协程
    if isSystemGoroutine(newg, false) {
        sched.ngsys.Add(1)
    } else {
        // Only user goroutines inherit pprof labels.
        if mp.curg != nil {
            newg.labels = mp.curg.labels
        }
        if goroutineProfile.active {
            // A concurrent goroutine profile is running. It should include
            // exactly the set of goroutines that were alive when the goroutine
            // profiler first stopped the world. That does not include newg, so
            // mark it as not needing a profile before transitioning it from
            // _Gdead.
            newg.goroutineProfiled.Store(goroutineProfileSatisfied)
        }
    }
    // Track initial transition?
    newg.trackingSeq = uint8(fastrand())
    if newg.trackingSeq%gTrackingPeriod == 0 {
        newg.tracking = true
    }
    // 将g更换为可运行状态
    casgstatus(newg, _Gdead, _Grunnable)
    gcController.addScannableStack(pp, int64(newg.stack.hi-newg.stack.lo))

    // 分配goid
    if pp.goidcache == pp.goidcacheend {
        // Sched.goidgen is the last allocated id,
        // this batch must be [sched.goidgen+1, sched.goidgen+GoidCacheBatch].
        // At startup sched.goidgen=0, so main goroutine receives goid=1.
        // Sched.goidgen 为最后一个分配的 id，相当于一个全局计数器
        // 这一批必须为 [sched.goidgen+1, sched.goidgen+GoidCacheBatch].
        // 启动时 sched.goidgen=0, 因此主 Goroutine 的 goid 为 1
        pp.goidcache = sched.goidgen.Add(_GoidCacheBatch)
        pp.goidcache -= _GoidCacheBatch - 1
        pp.goidcacheend = pp.goidcache + _GoidCacheBatch
    }
    newg.goid = pp.goidcache
    pp.goidcache++
    if raceenabled {
        newg.racectx = racegostart(callerpc)
        newg.raceignore = 0
        if newg.labels != nil {
            // See note in proflabel.go on labelSync's role in synchronizing
            // with the reads in the signal handler.
            racereleasemergeg(newg, unsafe.Pointer(&labelSync))
        }
    }
    if trace.enabled {
        traceGoCreate(newg, newg.startpc)
    }
    releasem(mp)

    // 返回新创建的g
    return newg
}

// saveAncestors copies previous ancestors of the given caller g and
// includes infor for the current caller into a new set of tracebacks for
// a g being created.
func saveAncestors(callergp *g) *[]ancestorInfo {
    // Copy all prior info, except for the root goroutine (goid 0).
    if debug.tracebackancestors <= 0 || callergp.goid == 0 {
        return nil
    }
    var callerAncestors []ancestorInfo
    if callergp.ancestors != nil {
        callerAncestors = *callergp.ancestors
    }
    n := int32(len(callerAncestors)) + 1
    if n > debug.tracebackancestors {
        n = debug.tracebackancestors
    }
    ancestors := make([]ancestorInfo, n)
    copy(ancestors[1:], callerAncestors)

    var pcs [_TracebackMaxFrames]uintptr
    npcs := gcallers(callergp, 0, pcs[:])
    ipcs := make([]uintptr, npcs)
    copy(ipcs, pcs[:])
    ancestors[0] = ancestorInfo{
        pcs:  ipcs,
        goid: callergp.goid,
        gopc: callergp.gopc,
    }

    ancestorsp := new([]ancestorInfo)
    *ancestorsp = ancestors
    return ancestorsp
}

// Put on gfree list.
// If local list is too long, transfer a batch to the global list.
func gfput(pp *p, gp *g) {
    if readgstatus(gp) != _Gdead {
        throw("gfput: bad status (not Gdead)")
    }

    stksize := gp.stack.hi - gp.stack.lo

    if stksize != uintptr(startingStackSize) {
        // non-standard stack size - free it.
        stackfree(gp.stack)
        gp.stack.lo = 0
        gp.stack.hi = 0
        gp.stackguard0 = 0
    }

    pp.gFree.push(gp)
    pp.gFree.n++
    if pp.gFree.n >= 64 {
        var (
            inc      int32
            stackQ   gQueue
            noStackQ gQueue
        )
        for pp.gFree.n >= 32 {
            gp := pp.gFree.pop()
            pp.gFree.n--
            if gp.stack.lo == 0 {
                noStackQ.push(gp)
            } else {
                stackQ.push(gp)
            }
            inc++
        }
        lock(&sched.gFree.lock)
        sched.gFree.noStack.pushAll(noStackQ)
        sched.gFree.stack.pushAll(stackQ)
        sched.gFree.n += inc
        unlock(&sched.gFree.lock)
    }
}

// Get from gfree list.
// If local list is empty, grab a batch from global list.
func gfget(pp *p) *g {
retry:
    if pp.gFree.empty() && (!sched.gFree.stack.empty() || !sched.gFree.noStack.empty()) {
        lock(&sched.gFree.lock)
        // Move a batch of free Gs to the P.
        for pp.gFree.n < 32 {
            // Prefer Gs with stacks.
            gp := sched.gFree.stack.pop()
            if gp == nil {
                gp = sched.gFree.noStack.pop()
                if gp == nil {
                    break
                }
            }
            sched.gFree.n--
            pp.gFree.push(gp)
            pp.gFree.n++
        }
        unlock(&sched.gFree.lock)
        goto retry
    }
    gp := pp.gFree.pop()
    if gp == nil {
        return nil
    }
    pp.gFree.n--
    if gp.stack.lo != 0 && gp.stack.hi-gp.stack.lo != uintptr(startingStackSize) {
        // Deallocate old stack. We kept it in gfput because it was the
        // right size when the goroutine was put on the free list, but
        // the right size has changed since then.
        systemstack(func() {
            stackfree(gp.stack)
            gp.stack.lo = 0
            gp.stack.hi = 0
            gp.stackguard0 = 0
        })
    }
    if gp.stack.lo == 0 {
        // Stack was deallocated in gfput or just above. Allocate a new one.
        systemstack(func() {
            gp.stack = stackalloc(startingStackSize)
        })
        gp.stackguard0 = gp.stack.lo + _StackGuard
    } else {
        if raceenabled {
            racemalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
        }
        if msanenabled {
            msanmalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
        }
        if asanenabled {
            asanunpoison(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
        }
    }
    return gp
}

// Purge all cached G's from gfree list to the global list.
func gfpurge(pp *p) {
    var (
        inc      int32
        stackQ   gQueue
        noStackQ gQueue
    )
    for !pp.gFree.empty() {
        gp := pp.gFree.pop()
        pp.gFree.n--
        if gp.stack.lo == 0 {
            noStackQ.push(gp)
        } else {
            stackQ.push(gp)
        }
        inc++
    }
    lock(&sched.gFree.lock)
    sched.gFree.noStack.pushAll(noStackQ)
    sched.gFree.stack.pushAll(stackQ)
    sched.gFree.n += inc
    unlock(&sched.gFree.lock)
}

// Breakpoint executes a breakpoint trap.
func Breakpoint() {
    breakpoint()
}

// dolockOSThread is called by LockOSThread and lockOSThread below
// after they modify m.locked. Do not allow preemption during this call,
// or else the m might be different in this function than in the caller.
//
// dolockOSThread 在修改 m.locked 后由 LockOSThread 和 lockOSThread 调用。
// 在此调用期间不允许抢占，否则此函数中的 m 可能与调用者中的 m 不同。
//go:nosplit
func dolockOSThread() {
    if GOARCH == "wasm" {
        return // no threads on wasm yet
    }
    gp := getg()
    // 将g和m互相锁定
    gp.m.lockedg.set(gp)
    gp.lockedm.set(gp.m)
}

//go:nosplit

// LockOSThread wires the calling goroutine to its current operating system thread.
// The calling goroutine will always execute in that thread,
// and no other goroutine will execute in it,
// until the calling goroutine has made as many calls to
// UnlockOSThread as to LockOSThread.
// If the calling goroutine exits without unlocking the thread,
// the thread will be terminated.
//
// All init functions are run on the startup thread. Calling LockOSThread
// from an init function will cause the main function to be invoked on
// that thread.
//
// A goroutine should call LockOSThread before calling OS services or
// non-Go library functions that depend on per-thread state.
func LockOSThread() {
    if atomic.Load(&newmHandoff.haveTemplateThread) == 0 && GOOS != "plan9" {
        // If we need to start a new thread from the locked
        // thread, we need the template thread. Start it now
        // while we're in a known-good state.
        // 开始一个模版线程
        startTemplateThread()
    }
    gp := getg()
    gp.m.lockedExt++
    if gp.m.lockedExt == 0 {
        gp.m.lockedExt--
        panic("LockOSThread nesting overflow")
    }
    dolockOSThread()
}

//go:nosplit
func lockOSThread() {
    getg().m.lockedInt++
    dolockOSThread()
}

// dounlockOSThread is called by UnlockOSThread and unlockOSThread below
// after they update m->locked. Do not allow preemption during this call,
// or else the m might be in different in this function than in the caller.
//
// dounlockOSThread 在更新 m->locked 后由 UnlockOSThread 和 unlockOSThread 调用。
// 在此调用期间不允许抢占，否则此函数中的 m 可能与调用者中的 m 不同。
//go:nosplit
func dounlockOSThread() {
    if GOARCH == "wasm" {
        return // no threads on wasm yet
    }
    gp := getg()
    if gp.m.lockedInt != 0 || gp.m.lockedExt != 0 {
        return
    }
    // 锁字段清零
    gp.m.lockedg = 0
    gp.lockedm = 0
}

//go:nosplit

// UnlockOSThread undoes an earlier call to LockOSThread.
// If this drops the number of active LockOSThread calls on the
// calling goroutine to zero, it unwires the calling goroutine from
// its fixed operating system thread.
// If there are no active LockOSThread calls, this is a no-op.
//
// Before calling UnlockOSThread, the caller must ensure that the OS
// thread is suitable for running other goroutines. If the caller made
// any permanent changes to the state of the thread that would affect
// other goroutines, it should not call this function and thus leave
// the goroutine locked to the OS thread until the goroutine (and
// hence the thread) exits.
func UnlockOSThread() {
    gp := getg()
    if gp.m.lockedExt == 0 {
        return
    }
    // 减少计数
    gp.m.lockedExt--
    dounlockOSThread()
}

//go:nosplit
func unlockOSThread() {
    gp := getg()
    if gp.m.lockedInt == 0 {
        systemstack(badunlockosthread)
    }
    // 减少计数
    gp.m.lockedInt--
    dounlockOSThread()
}

func badunlockosthread() {
    throw("runtime: internal error: misuse of lockOSThread/unlockOSThread")
}

func gcount() int32 {
    n := int32(atomic.Loaduintptr(&allglen)) - sched.gFree.n - sched.ngsys.Load()
    for _, pp := range allp {
        n -= pp.gFree.n
    }

    // All these variables can be changed concurrently, so the result can be inconsistent.
    // But at least the current goroutine is running.
    if n < 1 {
        n = 1
    }
    return n
}

func mcount() int32 {
    return int32(sched.mnext - sched.nmfreed)
}

var prof struct {
    signalLock atomic.Uint32

    // Must hold signalLock to write. Reads may be lock-free, but
    // signalLock should be taken to synchronize with changes.
    hz atomic.Int32
}

func _System()                    { _System() }
func _ExternalCode()              { _ExternalCode() }
func _LostExternalCode()          { _LostExternalCode() }
func _GC()                        { _GC() }
func _LostSIGPROFDuringAtomic64() { _LostSIGPROFDuringAtomic64() }
func _VDSO()                      { _VDSO() }

// Called if we receive a SIGPROF signal.
// Called by the signal handler, may run during STW.
//
//go:nowritebarrierrec
func sigprof(pc, sp, lr uintptr, gp *g, mp *m) {
    if prof.hz.Load() == 0 {
        return
    }

    // If mp.profilehz is 0, then profiling is not enabled for this thread.
    // We must check this to avoid a deadlock between setcpuprofilerate
    // and the call to cpuprof.add, below.
    if mp != nil && mp.profilehz == 0 {
        return
    }

    // On mips{,le}/arm, 64bit atomics are emulated with spinlocks, in
    // runtime/internal/atomic. If SIGPROF arrives while the program is inside
    // the critical section, it creates a deadlock (when writing the sample).
    // As a workaround, create a counter of SIGPROFs while in critical section
    // to store the count, and pass it to sigprof.add() later when SIGPROF is
    // received from somewhere else (with _LostSIGPROFDuringAtomic64 as pc).
    if GOARCH == "mips" || GOARCH == "mipsle" || GOARCH == "arm" {
        if f := findfunc(pc); f.valid() {
            if hasPrefix(funcname(f), "runtime/internal/atomic") {
                cpuprof.lostAtomic++
                return
            }
        }
        if GOARCH == "arm" && goarm < 7 && GOOS == "linux" && pc&0xffff0000 == 0xffff0000 {
            // runtime/internal/atomic functions call into kernel
            // helpers on arm < 7. See
            // runtime/internal/atomic/sys_linux_arm.s.
            cpuprof.lostAtomic++
            return
        }
    }

    // Profiling runs concurrently with GC, so it must not allocate.
    // Set a trap in case the code does allocate.
    // Note that on windows, one thread takes profiles of all the
    // other threads, so mp is usually not getg().m.
    // In fact mp may not even be stopped.
    // See golang.org/issue/17165.
    getg().m.mallocing++

    var stk [maxCPUProfStack]uintptr
    n := 0
    if mp.ncgo > 0 && mp.curg != nil && mp.curg.syscallpc != 0 && mp.curg.syscallsp != 0 {
        cgoOff := 0
        // Check cgoCallersUse to make sure that we are not
        // interrupting other code that is fiddling with
        // cgoCallers.  We are running in a signal handler
        // with all signals blocked, so we don't have to worry
        // about any other code interrupting us.
        if mp.cgoCallersUse.Load() == 0 && mp.cgoCallers != nil && mp.cgoCallers[0] != 0 {
            for cgoOff < len(mp.cgoCallers) && mp.cgoCallers[cgoOff] != 0 {
                cgoOff++
            }
            copy(stk[:], mp.cgoCallers[:cgoOff])
            mp.cgoCallers[0] = 0
        }

        // Collect Go stack that leads to the cgo call.
        n = gentraceback(mp.curg.syscallpc, mp.curg.syscallsp, 0, mp.curg, 0, &stk[cgoOff], len(stk)-cgoOff, nil, nil, 0)
        if n > 0 {
            n += cgoOff
        }
    } else if usesLibcall() && mp.libcallg != 0 && mp.libcallpc != 0 && mp.libcallsp != 0 {
        // Libcall, i.e. runtime syscall on windows.
        // Collect Go stack that leads to the call.
        n = gentraceback(mp.libcallpc, mp.libcallsp, 0, mp.libcallg.ptr(), 0, &stk[n], len(stk[n:]), nil, nil, 0)
    } else if mp != nil && mp.vdsoSP != 0 {
        // VDSO call, e.g. nanotime1 on Linux.
        // Collect Go stack that leads to the call.
        n = gentraceback(mp.vdsoPC, mp.vdsoSP, 0, gp, 0, &stk[n], len(stk[n:]), nil, nil, _TraceJumpStack)
    } else {
        n = gentraceback(pc, sp, lr, gp, 0, &stk[0], len(stk), nil, nil, _TraceTrap|_TraceJumpStack)
    }

    if n <= 0 {
        // Normal traceback is impossible or has failed.
        // Account it against abstract "System" or "GC".
        n = 2
        if inVDSOPage(pc) {
            pc = abi.FuncPCABIInternal(_VDSO) + sys.PCQuantum
        } else if pc > firstmoduledata.etext {
            // "ExternalCode" is better than "etext".
            pc = abi.FuncPCABIInternal(_ExternalCode) + sys.PCQuantum
        }
        stk[0] = pc
        if mp.preemptoff != "" {
            stk[1] = abi.FuncPCABIInternal(_GC) + sys.PCQuantum
        } else {
            stk[1] = abi.FuncPCABIInternal(_System) + sys.PCQuantum
        }
    }

    if prof.hz.Load() != 0 {
        // Note: it can happen on Windows that we interrupted a system thread
        // with no g, so gp could nil. The other nil checks are done out of
        // caution, but not expected to be nil in practice.
        var tagPtr *unsafe.Pointer
        if gp != nil && gp.m != nil && gp.m.curg != nil {
            tagPtr = &gp.m.curg.labels
        }
        cpuprof.add(tagPtr, stk[:n])

        gprof := gp
        var pp *p
        if gp != nil && gp.m != nil {
            if gp.m.curg != nil {
                gprof = gp.m.curg
            }
            pp = gp.m.p.ptr()
        }
        traceCPUSample(gprof, pp, stk[:n])
    }
    getg().m.mallocing--
}

// setcpuprofilerate sets the CPU profiling rate to hz times per second.
// If hz <= 0, setcpuprofilerate turns off CPU profiling.
func setcpuprofilerate(hz int32) {
    // Force sane arguments.
    if hz < 0 {
        hz = 0
    }

    // Disable preemption, otherwise we can be rescheduled to another thread
    // that has profiling enabled.
    gp := getg()
    gp.m.locks++

    // Stop profiler on this thread so that it is safe to lock prof.
    // if a profiling signal came in while we had prof locked,
    // it would deadlock.
    setThreadCPUProfiler(0)

    for !prof.signalLock.CompareAndSwap(0, 1) {
        osyield()
    }
    if prof.hz.Load() != hz {
        setProcessCPUProfiler(hz)
        prof.hz.Store(hz)
    }
    prof.signalLock.Store(0)

    lock(&sched.lock)
    sched.profilehz = hz
    unlock(&sched.lock)

    if hz != 0 {
        setThreadCPUProfiler(hz)
    }

    gp.m.locks--
}

// init initializes pp, which may be a freshly allocated p or a
// previously destroyed p, and transitions it to status _Pgcstop.
func (pp *p) init(id int32) {
    // p 的 id 就是它在 allp 中的索引
    pp.id = id
    // 新创建的 p 处于 _Pgcstop 状态
    pp.status = _Pgcstop
    pp.sudogcache = pp.sudogbuf[:0]
    pp.deferpool = pp.deferpoolbuf[:0]
    pp.wbBuf.reset()
    // 为P分配cache对象
    if pp.mcache == nil {
        // 如果 old == 0 且 i == 0 说明这是引导阶段初始化第一个 p
        if id == 0 {
            if mcache0 == nil {
                throw("missing mcache?")
            }
            // Use the bootstrap mcache0. Only one P will get
            // mcache0: the one with ID 0.
            pp.mcache = mcache0
        } else {
            pp.mcache = allocmcache()
        }
    }
    if raceenabled && pp.raceprocctx == 0 {
        if id == 0 {
            pp.raceprocctx = raceprocctx0
            raceprocctx0 = 0 // bootstrap
        } else {
            pp.raceprocctx = raceproccreate()
        }
    }
    lockInit(&pp.timersLock, lockRankTimers)

    // This P may get timers when it starts running. Set the mask here
    // since the P may not go through pidleget (notably P 0 on startup).
    timerpMask.set(id)
    // Similarly, we may not go through pidleget before this P starts
    // running if it is P 0 on startup.
    idlepMask.clear(id)
}

// destroy releases all of the resources associated with pp and
// transitions it to status _Pdead.
//
// sched.lock must be held and the world must be stopped.
// 释放未使用的 P，一般情况下不会执行这段代码
func (pp *p) destroy() {
    assertLockHeld(&sched.lock)
    assertWorldStopped()

    // Move all runnable goroutines to the global queue
    // 将所有 runnable Goroutine 移动至全局队列
    for pp.runqhead != pp.runqtail {
        // Pop from tail of local queue
        // 从本地队列中 pop
        pp.runqtail--
        gp := pp.runq[pp.runqtail%uint32(len(pp.runq))].ptr()
        // Push onto head of global queue
        // push 到全局队列中
        globrunqputhead(gp)
    }
    if pp.runnext != 0 {
        globrunqputhead(pp.runnext.ptr())
        pp.runnext = 0
    }
    if len(pp.timers) > 0 {
        plocal := getg().m.p.ptr()
        // The world is stopped, but we acquire timersLock to
        // protect against sysmon calling timeSleepUntil.
        // This is the only case where we hold the timersLock of
        // more than one P, so there are no deadlock concerns.
        lock(&plocal.timersLock)
        lock(&pp.timersLock)
        moveTimers(plocal, pp.timers)
        pp.timers = nil
        pp.numTimers.Store(0)
        pp.deletedTimers.Store(0)
        pp.timer0When.Store(0)
        unlock(&pp.timersLock)
        unlock(&plocal.timersLock)
    }
    // Flush p's write barrier buffer.
    if gcphase != _GCoff {
        wbBufFlush1(pp)
        pp.gcw.dispose()
    }
    for i := range pp.sudogbuf {
        pp.sudogbuf[i] = nil
    }
    pp.sudogcache = pp.sudogbuf[:0]
    for j := range pp.deferpoolbuf {
        pp.deferpoolbuf[j] = nil
    }
    pp.deferpool = pp.deferpoolbuf[:0]
    systemstack(func() {
        for i := 0; i < pp.mspancache.len; i++ {
            // Safe to call since the world is stopped.
            mheap_.spanalloc.free(unsafe.Pointer(pp.mspancache.buf[i]))
        }
        pp.mspancache.len = 0
        lock(&mheap_.lock)
        pp.pcache.flush(&mheap_.pages)
        unlock(&mheap_.lock)
    })
    freemcache(pp.mcache)
    pp.mcache = nil
    // 将当前 P 的空闲的 G 复链转移到全局
    gfpurge(pp)
    traceProcFree(pp)
    if raceenabled {
        if pp.timerRaceCtx != 0 {
            // The race detector code uses a callback to fetch
            // the proc context, so arrange for that callback
            // to see the right thing.
            // This hack only works because we are the only
            // thread running.
            mp := getg().m
            phold := mp.p.ptr()
            mp.p.set(pp)

            racectxend(pp.timerRaceCtx)
            pp.timerRaceCtx = 0

            mp.p.set(phold)
        }
        raceprocdestroy(pp.raceprocctx)
        pp.raceprocctx = 0
    }
    pp.gcAssistTime = 0
    pp.status = _Pdead
}

// Change number of processors.
//
// sched.lock must be held, and the world must be stopped.
//
// gcworkbufs must not be being modified by either the GC or the write barrier
// code, so the GC must not be running if the number of Ps actually changes.
//
// Returns list of Ps with local work, they need to be scheduled by the caller.
func procresize(nprocs int32) *p {
    assertLockHeld(&sched.lock)
    assertWorldStopped()

    // 获取之前P的个数
    old := gomaxprocs
    if old < 0 || nprocs <= 0 {
        throw("procresize: invalid arg")
    }
    if trace.enabled {
        traceGomaxprocs(nprocs)
    }

    // update statistics
    // 记录一下修改P的时间
    now := nanotime()
    if sched.procresizetime != 0 {
        sched.totaltime += int64(old) * (now - sched.procresizetime)
    }
    sched.procresizetime = now

    maskWords := (nprocs + 31) / 32

    // Grow allp if necessary.
    // 必要时增加allp
    // 这个时候本质上是在检查用户代码是否有调用过 runtime.MAXGOPROCS 调整 p 的数量
    // 此处多一步检查是为了避免内部的锁，如果 nprocs 明显小于 allp 的可见数量（因为 len）
    // 则不需要进行加锁
    if nprocs > int32(len(allp)) {
        // Synchronize with retake, which could be running
        // concurrently since it doesn't run on a P.
        // 此处与 retake 同步，它可以同时运行，因为它不会在 P 上运行。
        lock(&allpLock)
        if nprocs <= int32(cap(allp)) {
            // 如果 nprocs 被调小了，扔掉多余的 p
            allp = allp[:nprocs]
        } else {
            // 否则（调大了）创建更多的 p
            nallp := make([]*p, nprocs)
            // Copy everything up to allp's cap so we
            // never lose old allocated Ps.
            // 将原有的 p 复制到新创建的 new all p 中，不浪费旧的 p
            copy(nallp, allp[:cap(allp)])
            allp = nallp
        }

        if maskWords <= int32(cap(idlepMask)) {
            idlepMask = idlepMask[:maskWords]
            timerpMask = timerpMask[:maskWords]
        } else {
            nidlepMask := make([]uint32, maskWords)
            // No need to copy beyond len, old Ps are irrelevant.
            copy(nidlepMask, idlepMask)
            idlepMask = nidlepMask

            ntimerpMask := make([]uint32, maskWords)
            copy(ntimerpMask, timerpMask)
            timerpMask = ntimerpMask
        }
        unlock(&allpLock)
    }

    // initialize new P's
    // 初始化新的P
    for i := old; i < nprocs; i++ {
        pp := allp[i]
        // 如果P是新创建的，则申请新的P对象
        if pp == nil {
            pp = new(p)
        }
        pp.init(i)
        // allp[i] = pp
        atomicstorep(unsafe.Pointer(&allp[i]), unsafe.Pointer(pp))
    }

    gp := getg()
    if gp.m.p != 0 && gp.m.p.ptr().id < nprocs {
        // continue to use the current P
        // 继续使用当前 P
        gp.m.p.ptr().status = _Prunning
        gp.m.p.ptr().mcache.prepareForSweep()
    } else {
        // 将第一个 P 抢过来给当前 G 的 M 进行绑定
        // release the current P and acquire allp[0].
        //
        // We must do this before destroying our current P
        // because p.destroy itself has write barriers, so we
        // need to do that from a valid P.
        // 释放当前P，因为已经失效
        if gp.m.p != 0 {
            if trace.enabled {
                // Pretend that we were descheduled
                // and then scheduled again to keep
                // the trace sane.
                traceGoSched()
                traceProcStop(gp.m.p.ptr())
            }
            gp.m.p.ptr().m = 0
        }
        gp.m.p = 0
        // 更新到allp[0]
        pp := allp[0]
        pp.m = 0
        pp.status = _Pidle
        acquirep(pp) // 直接将allp[0]绑定到当前M
        if trace.enabled {
            traceGoStart()
        }
    }

    // g.m.p is now set, so we no longer need mcache0 for bootstrapping.
    mcache0 = nil

    // release resources from unused P's
    // 从未使用的P释放资源
    for i := nprocs; i < old; i++ {
        pp := allp[i]
        pp.destroy()
        // can't free P itself because it can be referenced by an M in syscall
        // 不能释放 p 本身，因为他可能在 m 进入系统调用时被引用
    }

    // Trim allp.
    // 清理完毕后，修剪 allp, nprocs 个数之外的所有 P
    if int32(len(allp)) != nprocs {
        lock(&allpLock)
        allp = allp[:nprocs]
        idlepMask = idlepMask[:maskWords]
        timerpMask = timerpMask[:maskWords]
        unlock(&allpLock)
    }

    // 将没有本地任务的 P 放到空闲链表中
    var runnablePs *p
    for i := nprocs - 1; i >= 0; i-- {
        pp := allp[i]
        // 确保不是当前正在使用的 P
        if gp.m.p.ptr() == pp {
            continue
        }
        // 将 p 设为 idle
        pp.status = _Pidle
        if runqempty(pp) {
            // 放入 idle 链表
            pidleput(pp, now)
        } else {
            // 如果有本地任务，则为其绑定一个 M
            pp.m.set(mget())
            // 构建可运行的 p 链表
            pp.link.set(runnablePs)
            runnablePs = pp
        }
    }
    stealOrder.reset(uint32(nprocs))
    // gomaxprocs = nprocs
    var int32p *int32 = &gomaxprocs // make compiler check that gomaxprocs is an int32
    atomic.Store((*uint32)(unsafe.Pointer(int32p)), uint32(nprocs))
    if old != nprocs {
        // Notify the limiter that the amount of procs has changed.
        gcCPULimiter.resetCapacity(now, nprocs)
    }
    // 返回所有包含本地任务的 P 链表
    return runnablePs
}

// Associate p and the current m.
//
// This function is allowed to have write barriers even if the caller
// isn't because it immediately acquires pp.
//
// 将 p 和当前 m 关联起来。即使调用者没有，该函数也可以有写屏障，因为它立即获取 pp。
//go:yeswritebarrierrec
func acquirep(pp *p) {
    // Do the part that isn't allowed to have write barriers.
    wirep(pp)

    // Have p; write barriers now allowed.

    // Perform deferred mcache flush before this P can allocate
    // from a potentially stale mcache.
    pp.mcache.prepareForSweep()

    if trace.enabled {
        traceProcStart()
    }
}

// wirep is the first step of acquirep, which actually associates the
// current M to pp. This is broken out so we can disallow write
// barriers for this part, since we don't yet have a P.
//
// wirep 是 acquirep 的第一步，它实际上将当前的 M 关联到 pp。
// 这已被打破，因此我们可以禁止这部分的写屏障，因为我们还没有 P。
//go:nowritebarrierrec
//go:nosplit
func wirep(pp *p) {
    gp := getg()

    if gp.m.p != 0 {
        throw("wirep: already in go")
    }
    // 检查 m 是否正常，并检查要获取的 p 的状态
    if pp.m != 0 || pp.status != _Pidle {
        id := int64(0)
        if pp.m != 0 {
            id = pp.m.ptr().id
        }
        print("wirep: p->m=", pp.m, "(", id, ") p->status=", pp.status, "\n")
        throw("wirep: invalid p state")
    }
    // 将 p 绑定到 m，p 和 m 互相引用
    gp.m.p.set(pp)
    pp.m.set(gp.m)
    pp.status = _Prunning
}

// Disassociate p and the current m.
func releasep() *p {
    gp := getg()

    if gp.m.p == 0 {
        throw("releasep: invalid arg")
    }
    pp := gp.m.p.ptr()
    if pp.m.ptr() != gp.m || pp.status != _Prunning {
        print("releasep: m=", gp.m, " m->p=", gp.m.p.ptr(), " p->m=", hex(pp.m), " p->status=", pp.status, "\n")
        throw("releasep: invalid p state")
    }
    if trace.enabled {
        traceProcStop(gp.m.p.ptr())
    }
    gp.m.p = 0
    pp.m = 0
    pp.status = _Pidle
    return pp
}

func incidlelocked(v int32) {
    lock(&sched.lock)
    sched.nmidlelocked += v
    if v > 0 {
        checkdead()
    }
    unlock(&sched.lock)
}

// Check for deadlock situation.
// The check is based on number of running M's, if 0 -> deadlock.
// sched.lock must be held.
func checkdead() {
    assertLockHeld(&sched.lock)

    // For -buildmode=c-shared or -buildmode=c-archive it's OK if
    // there are no running goroutines. The calling program is
    // assumed to be running.
    if islibrary || isarchive {
        return
    }

    // If we are dying because of a signal caught on an already idle thread,
    // freezetheworld will cause all running threads to block.
    // And runtime will essentially enter into deadlock state,
    // except that there is a thread that will call exit soon.
    if panicking.Load() > 0 {
        return
    }

    // If we are not running under cgo, but we have an extra M then account
    // for it. (It is possible to have an extra M on Windows without cgo to
    // accommodate callbacks created by syscall.NewCallback. See issue #6751
    // for details.)
    var run0 int32
    if !iscgo && cgoHasExtraM {
        mp := lockextra(true)
        haveExtraM := extraMCount > 0
        unlockextra(mp)
        if haveExtraM {
            run0 = 1
        }
    }

    run := mcount() - sched.nmidle - sched.nmidlelocked - sched.nmsys
    if run > run0 {
        return
    }
    if run < 0 {
        print("runtime: checkdead: nmidle=", sched.nmidle, " nmidlelocked=", sched.nmidlelocked, " mcount=", mcount(), " nmsys=", sched.nmsys, "\n")
        throw("checkdead: inconsistent counts")
    }

    grunning := 0
    forEachG(func(gp *g) {
        if isSystemGoroutine(gp, false) {
            return
        }
        s := readgstatus(gp)
        switch s &^ _Gscan {
        case _Gwaiting,
            _Gpreempted:
            grunning++
        case _Grunnable,
            _Grunning,
            _Gsyscall:
            print("runtime: checkdead: find g ", gp.goid, " in status ", s, "\n")
            throw("checkdead: runnable g")
        }
    })
    if grunning == 0 { // possible if main goroutine calls runtime·Goexit()
        unlock(&sched.lock) // unlock so that GODEBUG=scheddetail=1 doesn't hang
        fatal("no goroutines (main called runtime.Goexit) - deadlock!")
    }

    // Maybe jump time forward for playground.
    if faketime != 0 {
        if when := timeSleepUntil(); when < maxWhen {
            faketime = when

            // Start an M to steal the timer.
            pp, _ := pidleget(faketime)
            if pp == nil {
                // There should always be a free P since
                // nothing is running.
                throw("checkdead: no p for timer")
            }
            mp := mget()
            if mp == nil {
                // There should always be a free M since
                // nothing is running.
                throw("checkdead: no m for timer")
            }
            // M must be spinning to steal. We set this to be
            // explicit, but since this is the only M it would
            // become spinning on its own anyways.
            sched.nmspinning.Add(1)
            mp.spinning = true
            mp.nextp.set(pp)
            notewakeup(&mp.park)
            return
        }
    }

    // There are no goroutines running, so we can look at the P's.
    for _, pp := range allp {
        if len(pp.timers) > 0 {
            return
        }
    }

    unlock(&sched.lock) // unlock so that GODEBUG=scheddetail=1 doesn't hang
    fatal("all goroutines are asleep - deadlock!")
}

// forcegcperiod is the maximum time in nanoseconds between garbage
// collections. If we go this long without a garbage collection, one
// is forced to run.
//
// This is a variable for testing purposes. It normally doesn't change.
var forcegcperiod int64 = 2 * 60 * 1e9

// needSysmonWorkaround is true if the workaround for
// golang.org/issue/42515 is needed on NetBSD.
var needSysmonWorkaround bool = false

// Always runs without a P, so write barriers are not allowed.
//
//go:nowritebarrierrec
func sysmon() {
    lock(&sched.lock)
    sched.nmsys++
    checkdead()
    unlock(&sched.lock)

    lasttrace := int64(0)
    idle := 0 // how many cycles in succession we had not wokeup somebody
    delay := uint32(0)

    for {
        if idle == 0 { // start with 20us sleep...
            delay = 20
        } else if idle > 50 { // start doubling the sleep after 1ms...
            delay *= 2
        }
        if delay > 10*1000 { // up to 10ms
            delay = 10 * 1000
        }
        usleep(delay)

        // sysmon should not enter deep sleep if schedtrace is enabled so that
        // it can print that information at the right time.
        //
        // It should also not enter deep sleep if there are any active P's so
        // that it can retake P's from syscalls, preempt long running G's, and
        // poll the network if all P's are busy for long stretches.
        //
        // It should wakeup from deep sleep if any P's become active either due
        // to exiting a syscall or waking up due to a timer expiring so that it
        // can resume performing those duties. If it wakes from a syscall it
        // resets idle and delay as a bet that since it had retaken a P from a
        // syscall before, it may need to do it again shortly after the
        // application starts work again. It does not reset idle when waking
        // from a timer to avoid adding system load to applications that spend
        // most of their time sleeping.
        now := nanotime()
        if debug.schedtrace <= 0 && (sched.gcwaiting.Load() || sched.npidle.Load() == gomaxprocs) {
            lock(&sched.lock)
            if sched.gcwaiting.Load() || sched.npidle.Load() == gomaxprocs {
                syscallWake := false
                next := timeSleepUntil()
                if next > now {
                    sched.sysmonwait.Store(true)
                    unlock(&sched.lock)
                    // Make wake-up period small enough
                    // for the sampling to be correct.
                    sleep := forcegcperiod / 2
                    if next-now < sleep {
                        sleep = next - now
                    }
                    shouldRelax := sleep >= osRelaxMinNS
                    if shouldRelax {
                        osRelax(true)
                    }
                    syscallWake = notetsleep(&sched.sysmonnote, sleep)
                    if shouldRelax {
                        osRelax(false)
                    }
                    lock(&sched.lock)
                    sched.sysmonwait.Store(false)
                    noteclear(&sched.sysmonnote)
                }
                if syscallWake {
                    idle = 0
                    delay = 20
                }
            }
            unlock(&sched.lock)
        }

        lock(&sched.sysmonlock)
        // Update now in case we blocked on sysmonnote or spent a long time
        // blocked on schedlock or sysmonlock above.
        now = nanotime()

        // trigger libc interceptors if needed
        if *cgo_yield != nil {
            asmcgocall(*cgo_yield, nil)
        }
        // poll network if not polled for more than 10ms
        lastpoll := sched.lastpoll.Load()
        if netpollinited() && lastpoll != 0 && lastpoll+10*1000*1000 < now {
            sched.lastpoll.CompareAndSwap(lastpoll, now)
            list := netpoll(0) // non-blocking - returns list of goroutines
            if !list.empty() {
                // Need to decrement number of idle locked M's
                // (pretending that one more is running) before injectglist.
                // Otherwise it can lead to the following situation:
                // injectglist grabs all P's but before it starts M's to run the P's,
                // another M returns from syscall, finishes running its G,
                // observes that there is no work to do and no other running M's
                // and reports deadlock.
                incidlelocked(-1)
                injectglist(&list)
                incidlelocked(1)
            }
        }
        if GOOS == "netbsd" && needSysmonWorkaround {
            // netpoll is responsible for waiting for timer
            // expiration, so we typically don't have to worry
            // about starting an M to service timers. (Note that
            // sleep for timeSleepUntil above simply ensures sysmon
            // starts running again when that timer expiration may
            // cause Go code to run again).
            //
            // However, netbsd has a kernel bug that sometimes
            // misses netpollBreak wake-ups, which can lead to
            // unbounded delays servicing timers. If we detect this
            // overrun, then startm to get something to handle the
            // timer.
            //
            // See issue 42515 and
            // https://gnats.netbsd.org/cgi-bin/query-pr-single.pl?number=50094.
            if next := timeSleepUntil(); next < now {
                startm(nil, false, false)
            }
        }
        if scavenger.sysmonWake.Load() != 0 {
            // Kick the scavenger awake if someone requested it.
            scavenger.wake()
        }
        // retake P's blocked in syscalls
        // and preempt long running G's
        if retake(now) != 0 {
            idle = 0
        } else {
            idle++
        }
        // check if we need to force a GC
        if t := (gcTrigger{kind: gcTriggerTime, now: now}); t.test() && forcegc.idle.Load() {
            lock(&forcegc.lock)
            forcegc.idle.Store(false)
            var list gList
            list.push(forcegc.g)
            injectglist(&list)
            unlock(&forcegc.lock)
        }
        if debug.schedtrace > 0 && lasttrace+int64(debug.schedtrace)*1000000 <= now {
            lasttrace = now
            schedtrace(debug.scheddetail > 0)
        }
        unlock(&sched.sysmonlock)
    }
}

type sysmontick struct {
    schedtick   uint32
    schedwhen   int64
    syscalltick uint32
    syscallwhen int64
}

// forcePreemptNS is the time slice given to a G before it is
// preempted.
const forcePreemptNS = 10 * 1000 * 1000 // 10ms

func retake(now int64) uint32 {
    n := 0
    // Prevent allp slice changes. This lock will be completely
    // uncontended unless we're already stopping the world.
    lock(&allpLock)
    // We can't use a range loop over allp because we may
    // temporarily drop the allpLock. Hence, we need to re-fetch
    // allp each time around the loop.
    for i := 0; i < len(allp); i++ {
        pp := allp[i]
        if pp == nil {
            // This can happen if procresize has grown
            // allp but not yet created new Ps.
            continue
        }
        pd := &pp.sysmontick
        s := pp.status
        sysretake := false
        if s == _Prunning || s == _Psyscall {
            // Preempt G if it's running for too long.
            t := int64(pp.schedtick)
            if int64(pd.schedtick) != t {
                pd.schedtick = uint32(t)
                pd.schedwhen = now
            } else if pd.schedwhen+forcePreemptNS <= now {
                preemptone(pp)
                // In case of syscall, preemptone() doesn't
                // work, because there is no M wired to P.
                sysretake = true
            }
        }
        if s == _Psyscall {
            // Retake P from syscall if it's there for more than 1 sysmon tick (at least 20us).
            t := int64(pp.syscalltick)
            if !sysretake && int64(pd.syscalltick) != t {
                pd.syscalltick = uint32(t)
                pd.syscallwhen = now
                continue
            }
            // On the one hand we don't want to retake Ps if there is no other work to do,
            // but on the other hand we want to retake them eventually
            // because they can prevent the sysmon thread from deep sleep.
            if runqempty(pp) && sched.nmspinning.Load()+sched.npidle.Load() > 0 && pd.syscallwhen+10*1000*1000 > now {
                continue
            }
            // Drop allpLock so we can take sched.lock.
            unlock(&allpLock)
            // Need to decrement number of idle locked M's
            // (pretending that one more is running) before the CAS.
            // Otherwise the M from which we retake can exit the syscall,
            // increment nmidle and report deadlock.
            incidlelocked(-1)
            if atomic.Cas(&pp.status, s, _Pidle) {
                if trace.enabled {
                    traceGoSysBlock(pp)
                    traceProcStop(pp)
                }
                n++
                pp.syscalltick++
                handoffp(pp)
            }
            incidlelocked(1)
            lock(&allpLock)
        }
    }
    unlock(&allpLock)
    return uint32(n)
}

// Tell all goroutines that they have been preempted and they should stop.
// This function is purely best-effort. It can fail to inform a goroutine if a
// processor just started running it.
// No locks need to be held.
// Returns true if preemption request was issued to at least one goroutine.
func preemptall() bool {
    res := false
    for _, pp := range allp {
        if pp.status != _Prunning {
            continue
        }
        if preemptone(pp) {
            res = true
        }
    }
    return res
}

// Tell the goroutine running on processor P to stop.
// This function is purely best-effort. It can incorrectly fail to inform the
// goroutine. It can inform the wrong goroutine. Even if it informs the
// correct goroutine, that goroutine might ignore the request if it is
// simultaneously executing newstack.
// No lock needs to be held.
// Returns true if preemption request was issued.
// The actual preemption will happen at some point in the future
// and will be indicated by the gp->status no longer being
// Grunning
func preemptone(pp *p) bool {
    mp := pp.m.ptr()
    if mp == nil || mp == getg().m {
        return false
    }
    gp := mp.curg
    if gp == nil || gp == mp.g0 {
        return false
    }

    gp.preempt = true

    // Every call in a goroutine checks for stack overflow by
    // comparing the current stack pointer to gp->stackguard0.
    // Setting gp->stackguard0 to StackPreempt folds
    // preemption into the normal stack overflow check.
    gp.stackguard0 = stackPreempt

    // Request an async preemption of this P.
    if preemptMSupported && debug.asyncpreemptoff == 0 {
        pp.preempt = true
        preemptM(mp)
    }

    return true
}

var starttime int64

func schedtrace(detailed bool) {
    now := nanotime()
    if starttime == 0 {
        starttime = now
    }

    lock(&sched.lock)
    print("SCHED ", (now-starttime)/1e6, "ms: gomaxprocs=", gomaxprocs, " idleprocs=", sched.npidle.Load(), " threads=", mcount(), " spinningthreads=", sched.nmspinning.Load(), " needspinning=", sched.needspinning.Load(), " idlethreads=", sched.nmidle, " runqueue=", sched.runqsize)
    if detailed {
        print(" gcwaiting=", sched.gcwaiting.Load(), " nmidlelocked=", sched.nmidlelocked, " stopwait=", sched.stopwait, " sysmonwait=", sched.sysmonwait.Load(), "\n")
    }
    // We must be careful while reading data from P's, M's and G's.
    // Even if we hold schedlock, most data can be changed concurrently.
    // E.g. (p->m ? p->m->id : -1) can crash if p->m changes from non-nil to nil.
    for i, pp := range allp {
        mp := pp.m.ptr()
        h := atomic.Load(&pp.runqhead)
        t := atomic.Load(&pp.runqtail)
        if detailed {
            print("  P", i, ": status=", pp.status, " schedtick=", pp.schedtick, " syscalltick=", pp.syscalltick, " m=")
            if mp != nil {
                print(mp.id)
            } else {
                print("nil")
            }
            print(" runqsize=", t-h, " gfreecnt=", pp.gFree.n, " timerslen=", len(pp.timers), "\n")
        } else {
            // In non-detailed mode format lengths of per-P run queues as:
            // [len1 len2 len3 len4]
            print(" ")
            if i == 0 {
                print("[")
            }
            print(t - h)
            if i == len(allp)-1 {
                print("]\n")
            }
        }
    }

    if !detailed {
        unlock(&sched.lock)
        return
    }

    for mp := allm; mp != nil; mp = mp.alllink {
        pp := mp.p.ptr()
        print("  M", mp.id, ": p=")
        if pp != nil {
            print(pp.id)
        } else {
            print("nil")
        }
        print(" curg=")
        if mp.curg != nil {
            print(mp.curg.goid)
        } else {
            print("nil")
        }
        print(" mallocing=", mp.mallocing, " throwing=", mp.throwing, " preemptoff=", mp.preemptoff, " locks=", mp.locks, " dying=", mp.dying, " spinning=", mp.spinning, " blocked=", mp.blocked, " lockedg=")
        if lockedg := mp.lockedg.ptr(); lockedg != nil {
            print(lockedg.goid)
        } else {
            print("nil")
        }
        print("\n")
    }

    forEachG(func(gp *g) {
        print("  G", gp.goid, ": status=", readgstatus(gp), "(", gp.waitreason.String(), ") m=")
        if gp.m != nil {
            print(gp.m.id)
        } else {
            print("nil")
        }
        print(" lockedm=")
        if lockedm := gp.lockedm.ptr(); lockedm != nil {
            print(lockedm.id)
        } else {
            print("nil")
        }
        print("\n")
    })
    unlock(&sched.lock)
}

// schedEnableUser enables or disables the scheduling of user
// goroutines.
//
// This does not stop already running user goroutines, so the caller
// should first stop the world when disabling user goroutines.
func schedEnableUser(enable bool) {
    lock(&sched.lock)
    if sched.disable.user == !enable {
        unlock(&sched.lock)
        return
    }
    sched.disable.user = !enable
    if enable {
        n := sched.disable.n
        sched.disable.n = 0
        globrunqputbatch(&sched.disable.runnable, n)
        unlock(&sched.lock)
        for ; n != 0 && sched.npidle.Load() != 0; n-- {
            startm(nil, false, false)
        }
    } else {
        unlock(&sched.lock)
    }
}

// schedEnabled reports whether gp should be scheduled. It returns
// false is scheduling of gp is disabled.
//
// sched.lock must be held.
func schedEnabled(gp *g) bool {
    assertLockHeld(&sched.lock)

    if sched.disable.user {
        return isSystemGoroutine(gp, true)
    }
    return true
}

// Put mp on midle list.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func mput(mp *m) {
    assertLockHeld(&sched.lock)

    mp.schedlink = sched.midle
    sched.midle.set(mp)
    sched.nmidle++
    checkdead()
}

// Try to get an m from midle list.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func mget() *m {
    assertLockHeld(&sched.lock)

    mp := sched.midle.ptr()
    if mp != nil {
        sched.midle = mp.schedlink
        sched.nmidle--
    }
    return mp
}

// Put gp on the global runnable queue.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func globrunqput(gp *g) {
    assertLockHeld(&sched.lock)

    sched.runq.pushBack(gp)
    sched.runqsize++
}

// Put gp at the head of the global runnable queue.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func globrunqputhead(gp *g) {
    assertLockHeld(&sched.lock)

    sched.runq.push(gp)
    sched.runqsize++
}

// Put a batch of runnable goroutines on the global runnable queue.
// This clears *batch.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func globrunqputbatch(batch *gQueue, n int32) {
    assertLockHeld(&sched.lock)

    sched.runq.pushBackAll(*batch)
    sched.runqsize += n
    *batch = gQueue{}
}

// Try get a batch of G's from the global runnable queue.
// sched.lock must be held.
// 在 P（处理器）的本地队列中放入一些 Goroutine 并返回其中一个可运行的 Goroutine
func globrunqget(pp *p, max int32) *g {
    // 全局可运行队列的操作需要加锁以确保线程安全。
    assertLockHeld(&sched.lock)

    // 全局队列中没有g，直接返回
    if sched.runqsize == 0 {
        return nil
    }

    // 计算要获取的协程数量
    // 根据全局队列大小和 GOMAXPROCS 的值估算要获取的 Goroutine 数量，
    // 保证每个处理器 (P) 能够获取到合理数量的 Goroutine。
    n := sched.runqsize/gomaxprocs + 1
    // 比较 n 和全局队列的大小，确保不会超过实际的 Goroutine 数量。
    if n > sched.runqsize {
        n = sched.runqsize
    }
    // 如果传入的 max 参数限制了获取的数量，则按 max 进行限制。
    if max > 0 && n > max {
        n = max
    }
    // 如果要获取的数量超过了当前 P 本地运行队列的一半长度，则也要进行限制，
    // 避免过多的 Goroutine 堆积在一个 P 的本地队列中。
    if n > int32(len(pp.runq))/2 {
        n = int32(len(pp.runq)) / 2
    }

    // 更新全局队列的大小
    sched.runqsize -= n

    // 从全局队列中取出 Goroutine
    // 先从全局队列中弹出一个 Goroutine，赋值给 gp，这个 Goroutine 将会返回作为结果。
    gp := sched.runq.pop()
    n--
    // 然后继续从全局队列中弹出剩余的 Goroutine（n-1 个），将它们放入当前 P 的本地队列中。
    for ; n > 0; n-- {
        gp1 := sched.runq.pop()
        runqput(pp, gp1, false)
    }
    return gp
}

// pMask is an atomic bitstring with one bit per P.
type pMask []uint32

// read returns true if P id's bit is set.
func (p pMask) read(id uint32) bool {
    word := id / 32
    mask := uint32(1) << (id % 32)
    return (atomic.Load(&p[word]) & mask) != 0
}

// set sets P id's bit.
func (p pMask) set(id int32) {
    word := id / 32
    mask := uint32(1) << (id % 32)
    atomic.Or(&p[word], mask)
}

// clear clears P id's bit.
func (p pMask) clear(id int32) {
    word := id / 32
    mask := uint32(1) << (id % 32)
    atomic.And(&p[word], ^mask)
}

// updateTimerPMask clears pp's timer mask if it has no timers on its heap.
//
// Ideally, the timer mask would be kept immediately consistent on any timer
// operations. Unfortunately, updating a shared global data structure in the
// timer hot path adds too much overhead in applications frequently switching
// between no timers and some timers.
//
// As a compromise, the timer mask is updated only on pidleget / pidleput. A
// running P (returned by pidleget) may add a timer at any time, so its mask
// must be set. An idle P (passed to pidleput) cannot add new timers while
// idle, so if it has no timers at that time, its mask may be cleared.
//
// Thus, we get the following effects on timer-stealing in findrunnable:
//
//   - Idle Ps with no timers when they go idle are never checked in findrunnable
//     (for work- or timer-stealing; this is the ideal case).
//   - Running Ps must always be checked.
//   - Idle Ps whose timers are stolen must continue to be checked until they run
//     again, even after timer expiration.
//
// When the P starts running again, the mask should be set, as a timer may be
// added at any time.
//
// TODO(prattmic): Additional targeted updates may improve the above cases.
// e.g., updating the mask when stealing a timer.
func updateTimerPMask(pp *p) {
    if pp.numTimers.Load() > 0 {
        return
    }

    // Looks like there are no timers, however another P may transiently
    // decrement numTimers when handling a timerModified timer in
    // checkTimers. We must take timersLock to serialize with these changes.
    lock(&pp.timersLock)
    if pp.numTimers.Load() == 0 {
        timerpMask.clear(pp.id)
    }
    unlock(&pp.timersLock)
}

// pidleput puts p on the _Pidle list. now must be a relatively recent call
// to nanotime or zero. Returns now or the current time if now was zero.
//
// This releases ownership of p. Once sched.lock is released it is no longer
// safe to use p.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func pidleput(pp *p, now int64) int64 {
    assertLockHeld(&sched.lock)

    if !runqempty(pp) {
        throw("pidleput: P has non-empty run queue")
    }
    if now == 0 {
        now = nanotime()
    }
    updateTimerPMask(pp) // clear if there are no timers.
    idlepMask.set(pp.id)
    pp.link = sched.pidle
    sched.pidle.set(pp)
    sched.npidle.Add(1)
    if !pp.limiterEvent.start(limiterEventIdle, now) {
        throw("must be able to track idle limiter event")
    }
    return now
}

// pidleget tries to get a p from the _Pidle list, acquiring ownership.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func pidleget(now int64) (*p, int64) {
    assertLockHeld(&sched.lock)

    pp := sched.pidle.ptr()
    if pp != nil {
        // Timer may get added at any time now.
        if now == 0 {
            now = nanotime()
        }
        timerpMask.set(pp.id)
        idlepMask.clear(pp.id)
        sched.pidle = pp.link
        sched.npidle.Add(-1)
        pp.limiterEvent.stop(limiterEventIdle, now)
    }
    return pp, now
}

// pidlegetSpinning tries to get a p from the _Pidle list, acquiring ownership.
// This is called by spinning Ms (or callers than need a spinning M) that have
// found work. If no P is available, this must synchronized with non-spinning
// Ms that may be preparing to drop their P without discovering this work.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func pidlegetSpinning(now int64) (*p, int64) {
    assertLockHeld(&sched.lock)

    pp, now := pidleget(now)
    if pp == nil {
        // See "Delicate dance" comment in findrunnable. We found work
        // that we cannot take, we must synchronize with non-spinning
        // Ms that may be preparing to drop their P.
        sched.needspinning.Store(1)
        return nil, now
    }

    return pp, now
}

// runqempty reports whether pp has no Gs on its local run queue.
// It never returns true spuriously.
// runqempty 报告 pp 的本地运行队列上是否没有 G。它永远不会虚假地返回 true。
func runqempty(pp *p) bool {
    // Defend against a race where 1) pp has G1 in runqnext but runqhead == runqtail,
    // 2) runqput on pp kicks G1 to the runq, 3) runqget on pp empties runqnext.
    // Simply observing that runqhead == runqtail and then observing that runqnext == nil
    // does not mean the queue is empty.
    for {
        head := atomic.Load(&pp.runqhead)
        tail := atomic.Load(&pp.runqtail)
        runnext := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&pp.runnext)))
        if tail == atomic.Load(&pp.runqtail) {
            return head == tail && runnext == 0
        }
    }
}

// To shake out latent assumptions about scheduling order,
// we introduce some randomness into scheduling decisions
// when running with the race detector.
// The need for this was made obvious by changing the
// (deterministic) scheduling order in Go 1.5 and breaking
// many poorly-written tests.
// With the randomness here, as long as the tests pass
// consistently with -race, they shouldn't have latent scheduling
// assumptions.
const randomizeScheduler = raceenabled

// runqput tries to put g on the local runnable queue.
// If next is false, runqput adds g to the tail of the runnable queue.
// If next is true, runqput puts g in the pp.runnext slot.
// If the run queue is full, runnext puts g on the global queue.
// Executed only by the owner P.
// 将协程放入处理器P本地可运行队列，如果队列已满，会将协程放入全局队列
// next为true时，将当前协程加入到优先队列，否则加入到运行队列末尾
func runqput(pp *p, gp *g, next bool) {
    // 如果调度器是随机化，并且next为true，则有50%的几率会将next设置为false。
    // 这用于在某些情形下打乱调度顺序，提升调度的随机性
    if randomizeScheduler && next && fastrandn(2) == 0 {
        next = false
    }

    if next {
        // next为true，代码尝试将gp放入runnext，runnext是一个特殊的槽位，表示下一个要运行的协程
    retryNext:
        oldnext := pp.runnext
        // cas操作确保线程安全
        if !pp.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp))) {
            // cas失败，会一直循环，直到成功。
            goto retryNext
        }
        // runnext是空的，这个槽位可以直接使用
        if oldnext == 0 {
            return
        }
        // Kick the old runnext out to the regular run queue.
        // 将旧的协程踢出
        gp = oldnext.ptr()
    }

    // 尝试将协程放入本地队列
retry:
    // 加载本地队列头
    h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with consumers
    // 获取队尾
    t := pp.runqtail
    if t-h < uint32(len(pp.runq)) {
        // 队列还有空间
        pp.runq[t%uint32(len(pp.runq))].set(gp)
        // 更新尾指针并释放
        atomic.StoreRel(&pp.runqtail, t+1) // store-release, makes the item available for consumption
        return
    }
    // 放入全局队列
    if runqputslow(pp, gp, h, t) {
        return
    }
    // the queue is not full, now the put above must succeed
    goto retry
}

// Put g and a batch of work from local runnable queue on global queue.
// Executed only by the owner P.
func runqputslow(pp *p, gp *g, h, t uint32) bool {
    var batch [len(pp.runq)/2 + 1]*g

    // First, grab a batch from local queue.
    n := t - h
    n = n / 2
    if n != uint32(len(pp.runq)/2) {
        throw("runqputslow: queue is not full")
    }
    for i := uint32(0); i < n; i++ {
        batch[i] = pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
    }
    if !atomic.CasRel(&pp.runqhead, h, h+n) { // cas-release, commits consume
        return false
    }
    batch[n] = gp

    if randomizeScheduler {
        for i := uint32(1); i <= n; i++ {
            j := fastrandn(i + 1)
            batch[i], batch[j] = batch[j], batch[i]
        }
    }

    // Link the goroutines.
    for i := uint32(0); i < n; i++ {
        batch[i].schedlink.set(batch[i+1])
    }
    var q gQueue
    q.head.set(batch[0])
    q.tail.set(batch[n])

    // Now put the batch on global queue.
    lock(&sched.lock)
    globrunqputbatch(&q, int32(n+1))
    unlock(&sched.lock)
    return true
}

// runqputbatch tries to put all the G's on q on the local runnable queue.
// If the queue is full, they are put on the global queue; in that case
// this will temporarily acquire the scheduler lock.
// Executed only by the owner P.
func runqputbatch(pp *p, q *gQueue, qsize int) {
    h := atomic.LoadAcq(&pp.runqhead)
    t := pp.runqtail
    n := uint32(0)
    for !q.empty() && t-h < uint32(len(pp.runq)) {
        gp := q.pop()
        pp.runq[t%uint32(len(pp.runq))].set(gp)
        t++
        n++
    }
    qsize -= int(n)

    if randomizeScheduler {
        off := func(o uint32) uint32 {
            return (pp.runqtail + o) % uint32(len(pp.runq))
        }
        for i := uint32(1); i < n; i++ {
            j := fastrandn(i + 1)
            pp.runq[off(i)], pp.runq[off(j)] = pp.runq[off(j)], pp.runq[off(i)]
        }
    }

    atomic.StoreRel(&pp.runqtail, t)
    if !q.empty() {
        lock(&sched.lock)
        globrunqputbatch(q, int32(qsize))
        unlock(&sched.lock)
    }
}

// Get g from local runnable queue.
// If inheritTime is true, gp should inherit the remaining time in the
// current time slice. Otherwise, it should start a new time slice.
// Executed only by the owner P.
// 从本地可运行队列中获取 g
// 如果 inheritTime 为 true，则 g 继承剩余的时间片
// 否则开始一个新的时间片。在所有者 P 上执行
func runqget(pp *p) (gp *g, inheritTime bool) {
    // If there's a runnext, it's the next G to run.
    // runnext是被优先调度的协程
    // 检查runnext是否可用
    next := pp.runnext
    // If the runnext is non-0 and the CAS fails, it could only have been stolen by another P,
    // because other Ps can race to set runnext to 0, but only the current P can set it to non-0.
    // Hence, there's no need to retry this CAS if it fails.
    if next != 0 && pp.runnext.cas(next, 0) {
        // 可用，则直接返回，并继承剩余时间片
        return next.ptr(), true
    }

    // 从本地队列中获取
    for {
        h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
        t := pp.runqtail
        // 链表为空
        if t == h {
            return nil, false
        }
        // 如果队列中有可用 Goroutine，则从本地队列中的位置 h % uint32(len(pp.runq)) 获取 Goroutine。
        gp := pp.runq[h%uint32(len(pp.runq))].ptr()
        // 原子地将 runqhead 加 1（消费一个 Goroutine）。该操作成功后，返回该 Goroutine，
        // 并且 inheritTime 为 false，表示这个 Goroutine 不继承时间片。
        if atomic.CasRel(&pp.runqhead, h, h+1) { // cas-release, commits consume
            return gp, false
        }
    }
}

// runqdrain drains the local runnable queue of pp and returns all goroutines in it.
// Executed only by the owner P.
func runqdrain(pp *p) (drainQ gQueue, n uint32) {
    oldNext := pp.runnext
    if oldNext != 0 && pp.runnext.cas(oldNext, 0) {
        drainQ.pushBack(oldNext.ptr())
        n++
    }

retry:
    h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
    t := pp.runqtail
    qn := t - h
    if qn == 0 {
        return
    }
    if qn > uint32(len(pp.runq)) { // read inconsistent h and t
        goto retry
    }

    if !atomic.CasRel(&pp.runqhead, h, h+qn) { // cas-release, commits consume
        goto retry
    }

    // We've inverted the order in which it gets G's from the local P's runnable queue
    // and then advances the head pointer because we don't want to mess up the statuses of G's
    // while runqdrain() and runqsteal() are running in parallel.
    // Thus we should advance the head pointer before draining the local P into a gQueue,
    // so that we can update any gp.schedlink only after we take the full ownership of G,
    // meanwhile, other P's can't access to all G's in local P's runnable queue and steal them.
    // See https://groups.google.com/g/golang-dev/c/0pTKxEKhHSc/m/6Q85QjdVBQAJ for more details.
    for i := uint32(0); i < qn; i++ {
        gp := pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
        drainQ.pushBack(gp)
        n++
    }
    return
}

// Grabs a batch of goroutines from pp's runnable queue into batch.
// Batch is a ring buffer starting at batchHead.
// Returns number of grabbed goroutines.
// Can be executed by any P.
func runqgrab(pp *p, batch *[256]guintptr, batchHead uint32, stealRunNextG bool) uint32 {
    for {
        h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
        t := atomic.LoadAcq(&pp.runqtail) // load-acquire, synchronize with the producer
        n := t - h
        n = n - n/2
        if n == 0 {
            if stealRunNextG {
                // Try to steal from pp.runnext.
                if next := pp.runnext; next != 0 {
                    if pp.status == _Prunning {
                        // Sleep to ensure that pp isn't about to run the g
                        // we are about to steal.
                        // The important use case here is when the g running
                        // on pp ready()s another g and then almost
                        // immediately blocks. Instead of stealing runnext
                        // in this window, back off to give pp a chance to
                        // schedule runnext. This will avoid thrashing gs
                        // between different Ps.
                        // A sync chan send/recv takes ~50ns as of time of
                        // writing, so 3us gives ~50x overshoot.
                        if GOOS != "windows" && GOOS != "openbsd" && GOOS != "netbsd" {
                            usleep(3)
                        } else {
                            // On some platforms system timer granularity is
                            // 1-15ms, which is way too much for this
                            // optimization. So just yield.
                            osyield()
                        }
                    }
                    if !pp.runnext.cas(next, 0) {
                        continue
                    }
                    batch[batchHead%uint32(len(batch))] = next
                    return 1
                }
            }
            return 0
        }
        if n > uint32(len(pp.runq)/2) { // read inconsistent h and t
            continue
        }
        for i := uint32(0); i < n; i++ {
            g := pp.runq[(h+i)%uint32(len(pp.runq))]
            batch[(batchHead+i)%uint32(len(batch))] = g
        }
        if atomic.CasRel(&pp.runqhead, h, h+n) { // cas-release, commits consume
            return n
        }
    }
}

// Steal half of elements from local runnable queue of p2
// and put onto local runnable queue of p.
// Returns one of the stolen elements (or nil if failed).
func runqsteal(pp, p2 *p, stealRunNextG bool) *g {
    t := pp.runqtail
    n := runqgrab(p2, &pp.runq, t, stealRunNextG)
    if n == 0 {
        return nil
    }
    n--
    gp := pp.runq[(t+n)%uint32(len(pp.runq))].ptr()
    if n == 0 {
        return gp
    }
    h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with consumers
    if t-h+n >= uint32(len(pp.runq)) {
        throw("runqsteal: runq overflow")
    }
    atomic.StoreRel(&pp.runqtail, t+n) // store-release, makes the item available for consumption
    return gp
}

// A gQueue is a dequeue of Gs linked through g.schedlink. A G can only
// be on one gQueue or gList at a time.
type gQueue struct {
    head guintptr
    tail guintptr
}

// empty reports whether q is empty.
func (q *gQueue) empty() bool {
    return q.head == 0
}

// push adds gp to the head of q.
func (q *gQueue) push(gp *g) {
    gp.schedlink = q.head
    q.head.set(gp)
    if q.tail == 0 {
        q.tail.set(gp)
    }
}

// pushBack adds gp to the tail of q.
func (q *gQueue) pushBack(gp *g) {
    gp.schedlink = 0
    if q.tail != 0 {
        q.tail.ptr().schedlink.set(gp)
    } else {
        q.head.set(gp)
    }
    q.tail.set(gp)
}

// pushBackAll adds all Gs in q2 to the tail of q. After this q2 must
// not be used.
func (q *gQueue) pushBackAll(q2 gQueue) {
    if q2.tail == 0 {
        return
    }
    q2.tail.ptr().schedlink = 0
    if q.tail != 0 {
        q.tail.ptr().schedlink = q2.head
    } else {
        q.head = q2.head
    }
    q.tail = q2.tail
}

// pop removes and returns the head of queue q. It returns nil if
// q is empty.
func (q *gQueue) pop() *g {
    gp := q.head.ptr()
    if gp != nil {
        q.head = gp.schedlink
        if q.head == 0 {
            q.tail = 0
        }
    }
    return gp
}

// popList takes all Gs in q and returns them as a gList.
func (q *gQueue) popList() gList {
    stack := gList{q.head}
    *q = gQueue{}
    return stack
}

// A gList is a list of Gs linked through g.schedlink. A G can only be
// on one gQueue or gList at a time.
// gList 是通过 g.schedlink 链接的 G 列表。一个 G 一次只能位于一个 gQueue 或 gList 上。
type gList struct {
    head guintptr
}

// empty reports whether l is empty.
func (l *gList) empty() bool {
    return l.head == 0
}

// push adds gp to the head of l.
func (l *gList) push(gp *g) {
    gp.schedlink = l.head
    l.head.set(gp)
}

// pushAll prepends all Gs in q to l.
func (l *gList) pushAll(q gQueue) {
    if !q.empty() {
        q.tail.ptr().schedlink = l.head
        l.head = q.head
    }
}

// pop removes and returns the head of l. If l is empty, it returns nil.
func (l *gList) pop() *g {
    gp := l.head.ptr()
    if gp != nil {
        l.head = gp.schedlink
    }
    return gp
}

//go:linkname setMaxThreads runtime/debug.setMaxThreads
func setMaxThreads(in int) (out int) {
    lock(&sched.lock)
    out = int(sched.maxmcount)
    if in > 0x7fffffff { // MaxInt32
        sched.maxmcount = 0x7fffffff
    } else {
        sched.maxmcount = int32(in)
    }
    checkmcount()
    unlock(&sched.lock)
    return
}

// 将当前协程与某个处理器P固定绑定的函数
// 通常用于防止协程在某些操作中被调度到其他处理器上，以确保线程本地状态的一致性
//go:nosplit
func procPin() int {
    gp := getg()
    mp := gp.m

    // 增加锁计数，锁住当前M，防止当前的协程被调度器抢占并调度到其他的M或P上
    mp.locks++
    // 返回M所绑定的P
    return int(mp.p.ptr().id)
}

// 解除协程与处理器P之间的绑定
//go:nosplit
func procUnpin() {
    gp := getg()
    gp.m.locks--
}

//go:linkname sync_runtime_procPin sync.runtime_procPin
//go:nosplit
func sync_runtime_procPin() int {
    return procPin()
}

//go:linkname sync_runtime_procUnpin sync.runtime_procUnpin
//go:nosplit
func sync_runtime_procUnpin() {
    procUnpin()
}

//go:linkname sync_atomic_runtime_procPin sync/atomic.runtime_procPin
//go:nosplit
func sync_atomic_runtime_procPin() int {
    return procPin()
}

//go:linkname sync_atomic_runtime_procUnpin sync/atomic.runtime_procUnpin
//go:nosplit
func sync_atomic_runtime_procUnpin() {
    procUnpin()
}

// Active spinning for sync.Mutex.
//
//go:linkname sync_runtime_canSpin sync.runtime_canSpin
//go:nosplit
func sync_runtime_canSpin(i int) bool {
    // sync.Mutex is cooperative, so we are conservative with spinning.
    // Spin only few times and only if running on a multicore machine and
    // GOMAXPROCS>1 and there is at least one other running P and local runq is empty.
    // As opposed to runtime mutex we don't do passive spinning here,
    // because there can be work on global runq or on other Ps.
    if i >= active_spin || ncpu <= 1 || gomaxprocs <= sched.npidle.Load()+sched.nmspinning.Load()+1 {
        return false
    }
    if p := getg().m.p.ptr(); !runqempty(p) {
        return false
    }
    return true
}

//go:linkname sync_runtime_doSpin sync.runtime_doSpin
//go:nosplit
func sync_runtime_doSpin() {
    procyield(active_spin_cnt)
}

var stealOrder randomOrder

// randomOrder/randomEnum are helper types for randomized work stealing.
// They allow to enumerate all Ps in different pseudo-random orders without repetitions.
// The algorithm is based on the fact that if we have X such that X and GOMAXPROCS
// are coprime, then a sequences of (i + X) % GOMAXPROCS gives the required enumeration.
type randomOrder struct {
    count    uint32
    coprimes []uint32
}

type randomEnum struct {
    i     uint32
    count uint32
    pos   uint32
    inc   uint32
}

func (ord *randomOrder) reset(count uint32) {
    ord.count = count
    ord.coprimes = ord.coprimes[:0]
    for i := uint32(1); i <= count; i++ {
        if gcd(i, count) == 1 {
            ord.coprimes = append(ord.coprimes, i)
        }
    }
}

func (ord *randomOrder) start(i uint32) randomEnum {
    return randomEnum{
        count: ord.count,
        pos:   i % ord.count,
        inc:   ord.coprimes[i/ord.count%uint32(len(ord.coprimes))],
    }
}

func (enum *randomEnum) done() bool {
    return enum.i == enum.count
}

func (enum *randomEnum) next() {
    enum.i++
    enum.pos = (enum.pos + enum.inc) % enum.count
}

func (enum *randomEnum) position() uint32 {
    return enum.pos
}

func gcd(a, b uint32) uint32 {
    for b != 0 {
        a, b = b, a%b
    }
    return a
}

// An initTask represents the set of initializations that need to be done for a package.
// Keep in sync with ../../test/initempty.go:initTask
type initTask struct {
    // TODO: pack the first 3 fields more tightly?
    state uintptr // 0 = uninitialized, 1 = in progress, 2 = done
    ndeps uintptr
    nfns  uintptr
    // followed by ndeps instances of an *initTask, one per package depended on
    // followed by nfns pcs, one per init function to run
}

// inittrace stores statistics for init functions which are
// updated by malloc and newproc when active is true.
var inittrace tracestat

type tracestat struct {
    active bool   // init tracing activation status
    id     uint64 // init goroutine id
    allocs uint64 // heap allocations
    bytes  uint64 // heap allocated bytes
}

func doInit(t *initTask) {
    switch t.state {
    case 2: // fully initialized
        return
    case 1: // initialization in progress
        throw("recursive call during initialization - linker skew")
    default: // not initialized yet
        t.state = 1 // initialization in progress

        for i := uintptr(0); i < t.ndeps; i++ {
            p := add(unsafe.Pointer(t), (3+i)*goarch.PtrSize)
            t2 := *(**initTask)(p)
            doInit(t2)
        }

        if t.nfns == 0 {
            t.state = 2 // initialization done
            return
        }

        var (
            start  int64
            before tracestat
        )

        if inittrace.active {
            start = nanotime()
            // Load stats non-atomically since tracinit is updated only by this init goroutine.
            before = inittrace
        }

        firstFunc := add(unsafe.Pointer(t), (3+t.ndeps)*goarch.PtrSize)
        for i := uintptr(0); i < t.nfns; i++ {
            p := add(firstFunc, i*goarch.PtrSize)
            f := *(*func())(unsafe.Pointer(&p))
            f()
        }

        if inittrace.active {
            end := nanotime()
            // Load stats non-atomically since tracinit is updated only by this init goroutine.
            after := inittrace

            f := *(*func())(unsafe.Pointer(&firstFunc))
            pkg := funcpkgpath(findfunc(abi.FuncPCABIInternal(f)))

            var sbuf [24]byte
            print("init ", pkg, " @")
            print(string(fmtNSAsMS(sbuf[:], uint64(start-runtimeInitTime))), " ms, ")
            print(string(fmtNSAsMS(sbuf[:], uint64(end-start))), " ms clock, ")
            print(string(itoa(sbuf[:], after.bytes-before.bytes)), " bytes, ")
            print(string(itoa(sbuf[:], after.allocs-before.allocs)), " allocs")
            print("\n")
        }

        t.state = 2 // initialization done
    }
}
