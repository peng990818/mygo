// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go channels.

// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement,
//  in which case the length of c.sendq and c.recvq is limited only by the
//  size of the select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.

import (
    "internal/abi"
    "runtime/internal/atomic"
    "runtime/internal/math"
    "unsafe"
)

const (
    maxAlign = 8
    // 通道头部大小
    hchanSize = unsafe.Sizeof(hchan{}) + uintptr(-int(unsafe.Sizeof(hchan{}))&(maxAlign-1)) // hchan的大小对齐
    debugChan = false
)

type hchan struct {
    qcount   uint           // total data in the queue 队列中所有数据数
    dataqsiz uint           // size of the circular queue 环形队列的大小
    buf      unsafe.Pointer // points to an array of dataqsiz elements 指向环形队列数组的指针
    elemsize uint16         // 元素大小
    closed   uint32         // 是否关闭
    elemtype *_type         // element type 元素类型
    sendx    uint           // send index 发送索引
    recvx    uint           // receive index 接收索引
    recvq    waitq          // list of recv waiters 接收等待列表
    sendq    waitq          // list of send waiters 发送等待列表

    // lock protects all fields in hchan, as well as several
    // fields in sudogs blocked on this channel.
    //
    // Do not change another G's status while holding this lock
    // (in particular, do not ready a G), as this can deadlock
    // with stack shrinking.
    lock mutex
}

type waitq struct {
    first *sudog
    last  *sudog
}

//go:linkname reflect_makechan reflect.makechan
func reflect_makechan(t *chantype, size int) *hchan {
    return makechan(t, size)
}

func makechan64(t *chantype, size int64) *hchan {
    // 确保了size在转换为int类型时不会发生溢出，由于makechan可能依赖于int类型来处理通道的容量，
    // 这个检查是必要的，以避免潜在的内存分配错误或其他逻辑错误，
    // 如果size超出int类型的表示范围，则会引发panic，避免不合法的通道创建
    if int64(int(size)) != size {
        panic(plainError("makechan: size out of range"))
    }

    return makechan(t, int(size))
}

func makechan(t *chantype, size int) *hchan {
    elem := t.elem

    // compiler checks this but be safe.
    // 通道元素的大小不能超过64KB
    // 虽然编译器会进行类型检查，但这里仍加一层检查，确保安全性
    if elem.size >= 1<<16 {
        throw("makechan: invalid channel element type")
    }
    // 检查通道的对齐情况
    // 检查通道的大小是否可以被最大对齐值整除或者检查通道元素的对齐值是否超过了最大对齐值
    if hchanSize%maxAlign != 0 || elem.align > maxAlign {
        throw("makechan: bad alignment")
    }

    // 计算元素大小与个数的乘积，返回结果和溢出标志
    mem, overflow := math.MulUintptr(elem.size, uintptr(size))
    // 溢出标志：乘积超出了uintptr类型的范围
    // 如果乘积mem大于了maxAlloc-hchanSize，说明需要分配的内存超出了最大允许分配的内存量。
    // size小于0，说明是一个无效通道
    if overflow || mem > maxAlloc-hchanSize || size < 0 {
        panic(plainError("makechan: size out of range"))
    }

    // Hchan does not contain pointers interesting for GC when elements stored in buf do not contain pointers.
    // buf points into the same allocation, elemtype is persistent.
    // SudoG's are referenced from their owning thread so they can't be collected.
    // TODO(dvyukov,rlh): Rethink when collector can move allocated objects.
    var c *hchan
    switch {
    case mem == 0:
        // Queue or element size is zero.
        // 队列或元素大小为0，仅需分配一个hchan结构体的内存
        c = (*hchan)(mallocgc(hchanSize, nil, true))
        // Race detector uses this location for synchronization.
        // 竟态检测
        c.buf = c.raceaddr()
    case elem.ptrdata == 0:
        // Elements do not contain pointers.
        // Allocate hchan and buf in one call.
        // 元素不包含指针
        // 一次性分配hchan和缓冲区buf的内存
        c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
        // 将buf指针设置为c指针后面的内存位置
        c.buf = add(unsafe.Pointer(c), hchanSize)
    default:
        // Elements contain pointers.
        // 包含指针，分别分配hchan和缓冲区buf的内存
        c = new(hchan)
        c.buf = mallocgc(mem, elem, true)
    }

    // 初始化通道结构体
    c.elemsize = uint16(elem.size)
    c.elemtype = elem
    c.dataqsiz = uint(size)
    // 初始化通道的锁
    lockInit(&c.lock, lockRankHchan)

    if debugChan {
        print("makechan: chan=", c, "; elemsize=", elem.size, "; dataqsiz=", size, "\n")
    }
    return c
}

// chanbuf(c, i) is pointer to the i'th slot in the buffer.
func chanbuf(c *hchan, i uint) unsafe.Pointer {
    return add(c.buf, uintptr(i)*uintptr(c.elemsize))
}

// full reports whether a send on c would block (that is, the channel is full).
// It uses a single word-sized read of mutable state, so although
// the answer is instantaneously true, the correct answer may have changed
// by the time the calling function receives the return value.
func full(c *hchan) bool {
    // c.dataqsiz is immutable (never written after the channel is created)
    // so it is safe to read at any time during channel operation.
    if c.dataqsiz == 0 {
        // Assumes that a pointer read is relaxed-atomic.
        return c.recvq.first == nil
    }
    // Assumes that a uint read is relaxed-atomic.
    return c.qcount == c.dataqsiz
}

// 向channel发送数据的实现
// entry point for c <- x from compiled code.
//
//go:nosplit
func chansend1(c *hchan, elem unsafe.Pointer) {
    chansend(c, elem, true, getcallerpc())
}

/*
 * generic single channel send/recv
 * If block is not nil,
 * then the protocol will not
 * sleep but return if it could
 * not complete.
 *
 * sleep can wake up with g.param == nil
 * when a channel involved in the sleep has
 * been closed.  it is easiest to loop and re-run
 * the operation; we'll see that it's now closed.
 */
// c 指向目标channel的指针
// ep 指向待发送数据的指针
// block 是否为阻塞操作
// callerpc 调用方的程序计数器
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
    // 向nil的channel发送数据，会调用gopark
    if c == nil {
        if !block {
            return false
        }
        // gopark会将当前的协程休眠，发生死锁崩溃
        gopark(nil, nil, waitReasonChanSendNilChan, traceEvGoStop, 2)
        throw("unreachable")
    }

    if debugChan {
        print("chansend: chan=", c, "\n")
    }

    // 如果启用了竟态检测，会记录相应的读操作
    if raceenabled {
        racereadpc(c.raceaddr(), callerpc, abi.FuncPCABIInternal(chansend))
    }

    // Fast path: check for failed non-blocking operation without acquiring the lock.
    //
    // After observing that the channel is not closed, we observe that the channel is
    // not ready for sending. Each of these observations is a single word-sized read
    // (first c.closed and second full()).
    // Because a closed channel cannot transition from 'ready for sending' to
    // 'not ready for sending', even if the channel is closed between the two observations,
    // they imply a moment between the two when the channel was both not yet closed
    // and not ready for sending. We behave as if we observed the channel at that moment,
    // and report that the send cannot proceed.
    //
    // It is okay if the reads are reordered here: if we observe that the channel is not
    // ready for sending and then observe that it is not closed, that implies that the
    // channel wasn't closed during the first observation. However, nothing here
    // guarantees forward progress. We rely on the side effects of lock release in
    // chanrecv() and closechan() to update this thread's view of c.closed and full().
    // 非阻塞操作并且没有关闭并且满了
    if !block && c.closed == 0 && full(c) {
        return false
    }

    // 启用性能分析，记录当前时间戳
    var t0 int64
    if blockprofilerate > 0 {
        t0 = cputicks()
    }

    // 加锁，确保并发安全
    lock(&c.lock)

    // 双重检验
    // channel关闭了，解锁并且panic
    if c.closed != 0 {
        unlock(&c.lock)
        panic(plainError("send on closed channel"))
    }

    // 检查是否有等待的接收者
    if sg := c.recvq.dequeue(); sg != nil {
        // Found a waiting receiver. We pass the value we want to send
        // directly to the receiver, bypassing the channel buffer (if any).
        // 有，直接将数据发送给接收者，绕过缓冲区，并解锁
        send(c, sg, ep, func() { unlock(&c.lock) }, 3)
        return true
    }

    if c.qcount < c.dataqsiz {
        // 有缓冲区空间
        // Space is available in the channel buffer. Enqueue the element to send.
        // 获取要拷贝到的缓冲区地址空间
        qp := chanbuf(c, c.sendx)
        if raceenabled {
            racenotify(c, c.sendx, nil)
        }
        // 将数据复制到缓冲区
        typedmemmove(c.elemtype, qp, ep)
        // 更新缓冲区计数和索引，解锁并返回
        c.sendx++
        if c.sendx == c.dataqsiz {
            c.sendx = 0
        }
        c.qcount++
        unlock(&c.lock)
        return true
    }

    // 非阻塞操作，没有空间，解锁返回
    if !block {
        unlock(&c.lock)
        return false
    }

    // 没有等待的并且也没有缓冲空间了则会阻塞协程
    // Block on the channel. Some receiver will complete our operation for us.
    gp := getg()

    // 创建sudog
    mysg := acquireSudog()
    mysg.releasetime = 0
    if t0 != 0 {
        mysg.releasetime = -1
    }
    // No stack splits between assigning elem and enqueuing mysg
    // on gp.waiting where copystack can find it.
    mysg.elem = ep
    mysg.waitlink = nil
    mysg.g = gp
    mysg.isSelect = false
    mysg.c = c
    gp.waiting = mysg
    gp.param = nil
    // 加入发送等待队列
    c.sendq.enqueue(mysg)
    // Signal to anyone trying to shrink our stack that we're about
    // to park on a channel. The window between when this G's status
    // changes and when we set gp.activeStackChans is not safe for
    // stack shrinking.
    // 将当前协程状态设置为等待，并将其挂起，等待被唤醒
    gp.parkingOnChan.Store(true)
    gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanSend, traceEvGoBlockSend, 2)
    // Ensure the value being sent is kept alive until the
    // receiver copies it out. The sudog has a pointer to the
    // stack object, but sudogs aren't considered as roots of the
    // stack tracer.
    // 确保待发送的数据在接收者复制之前不会被回收
    KeepAlive(ep)

    // someone woke us up.
    // 被唤醒后，检查等待队列是否被破坏
    if mysg != gp.waiting {
        throw("G waiting list is corrupted")
    }
    // 更新相应状态
    gp.waiting = nil
    gp.activeStackChans = false
    closed := !mysg.success
    gp.param = nil
    if mysg.releasetime > 0 {
        blockevent(mysg.releasetime-t0, 2)
    }
    mysg.c = nil
    // 释放
    releaseSudog(mysg)
    // channel 已经关闭 触发panic
    if closed {
        if c.closed == 0 {
            throw("chansend: spurious wakeup")
        }
        panic(plainError("send on closed channel"))
    }
    return true
}

// send processes a send operation on an empty channel c.
// The value ep sent by the sender is copied to the receiver sg.
// The receiver is then woken up to go on its merry way.
// Channel c must be empty and locked.  send unlocks c with unlockf.
// sg must already be dequeued from c.
// ep must be non-nil and point to the heap or the caller's stack.
func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
    // todo 启用竟态检测相关，待研究
    if raceenabled {
        if c.dataqsiz == 0 {
            racesync(c, sg)
        } else {
            // Pretend we go through the buffer, even though
            // we copy directly. Note that we need to increment
            // the head/tail locations only when raceenabled.
            racenotify(c, c.recvx, nil)
            racenotify(c, c.recvx, sg)
            c.recvx++
            if c.recvx == c.dataqsiz {
                c.recvx = 0
            }
            c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
        }
    }
    // 直接发送数据
    if sg.elem != nil {
        sendDirect(c.elemtype, sg, ep)
        sg.elem = nil
    }
    // 唤醒接收者
    gp := sg.g
    // 解锁channel
    unlockf()
    gp.param = unsafe.Pointer(sg)
    // 标志改为true，表示发送成功
    sg.success = true
    // 更新释放时间
    if sg.releasetime != 0 {
        sg.releasetime = cputicks()
    }
    // 唤醒接收者gp
    goready(gp, skip+1)
}

// Sends and receives on unbuffered or empty-buffered channels are the
// only operations where one running goroutine writes to the stack of
// another running goroutine. The GC assumes that stack writes only
// happen when the goroutine is running and are only done by that
// goroutine. Using a write barrier is sufficient to make up for
// violating that assumption, but the write barrier has to work.
// typedmemmove will call bulkBarrierPreWrite, but the target bytes
// are not in the heap, so that will not help. We arrange to call
// memmove and typeBitsBulkBarrier instead.

func sendDirect(t *_type, sg *sudog, src unsafe.Pointer) {
    // src is on our stack, dst is a slot on another stack.
    // src在当前goroutine的栈上，dst是另一个goroutine栈上的槽位。

    // Once we read sg.elem out of sg, it will no longer
    // be updated if the destination's stack gets copied (shrunk).
    // So make sure that no preemption points can happen between read & use.
    // 读取目标槽位的地址到dst。一旦读取出来，如果目标goroutine的栈被复制，例如栈缩小
    // sg.elem将不会被更新。因此，在读取和使用之间确保没有抢占点（即不允许当前goroutine被挂起）
    dst := sg.elem
    // todo 一种内存屏障，用于确保在内存复制操作之前正确处理写屏障
    typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
    // No need for cgo write barrier checks because dst is always
    // Go memory.
    // 将数据从src复制到dst。这是一种低级别的内存复制操作，不需要额外的cgo写屏障检查
    // dst始终是go内存
    memmove(dst, src, t.size)
}

func recvDirect(t *_type, sg *sudog, dst unsafe.Pointer) {
    // dst is on our stack or the heap, src is on another stack.
    // The channel is locked, so src will not move during this
    // operation.
    // 和sendDirect的情况差不多
    src := sg.elem
    typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
    memmove(dst, src, t.size)
}

// 关闭channel
func closechan(c *hchan) {
    if c == nil {
        // close一个空的channel会产生panic
        panic(plainError("close of nil channel"))
    }

    lock(&c.lock)
    if c.closed != 0 {
        unlock(&c.lock)
        // 重复关闭channel也会产生panic
        panic(plainError("close of closed channel"))
    }

    // 数据竞争检测，记录相关信息
    if raceenabled {
        callerpc := getcallerpc()
        racewritepc(c.raceaddr(), callerpc, abi.FuncPCABIInternal(closechan))
        racerelease(c.raceaddr())
    }

    // 设置关闭状态
    c.closed = 1

    // 定义一个goroutine列表
    var glist gList

    // release all readers
    // 释放所有等待接收的goroutine
    for {
        sg := c.recvq.dequeue()
        if sg == nil {
            break
        }
        if sg.elem != nil {
            // 清理
            typedmemclr(c.elemtype, sg.elem)
            sg.elem = nil
        }
        // 更新释放时间
        if sg.releasetime != 0 {
            sg.releasetime = cputicks()
        }
        gp := sg.g
        gp.param = unsafe.Pointer(sg)
        sg.success = false
        if raceenabled {
            raceacquireg(gp, c.raceaddr())
        }
        glist.push(gp)
    }

    // release all writers (they will panic)
    // 释放所有发送方
    for {
        sg := c.sendq.dequeue()
        if sg == nil {
            break
        }
        sg.elem = nil
        if sg.releasetime != 0 {
            sg.releasetime = cputicks()
        }
        gp := sg.g
        gp.param = unsafe.Pointer(sg)
        sg.success = false
        if raceenabled {
            raceacquireg(gp, c.raceaddr())
        }
        glist.push(gp)
    }
    unlock(&c.lock)

    // Ready all Gs now that we've dropped the channel lock.
    // 就绪所有的协程
    for !glist.empty() {
        gp := glist.pop()
        gp.schedlink = 0
        goready(gp, 3)
    }
}

// empty reports whether a read from c would block (that is, the channel is
// empty).  It uses a single atomic read of mutable state.
func empty(c *hchan) bool {
    // c.dataqsiz is immutable.
    if c.dataqsiz == 0 {
        return atomic.Loadp(unsafe.Pointer(&c.sendq.first)) == nil
    }
    return atomic.Loaduint(&c.qcount) == 0
}

// 从channel接收数据的实现
// entry points for <- c from compiled code.
//
//go:nosplit
func chanrecv1(c *hchan, elem unsafe.Pointer) {
    chanrecv(c, elem, true)
}

//go:nosplit
func chanrecv2(c *hchan, elem unsafe.Pointer) (received bool) {
    _, received = chanrecv(c, elem, true)
    return
}

// chanrecv receives on channel c and writes the received data to ep.
// ep may be nil, in which case received data is ignored.
// If block == false and no elements are available, returns (false, false).
// Otherwise, if c is closed, zeros *ep and returns (true, false).
// Otherwise, fills in *ep with an element and returns (true, true).
// A non-nil ep must point to the heap or the caller's stack.
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
    // raceenabled: don't need to check ep, as it is always on the stack
    // or is new memory allocated by reflect.

    if debugChan {
        print("chanrecv: chan=", c, "\n")
    }

    // nil的channel
    if c == nil {
        // 非阻塞模式，直接返回
        if !block {
            return
        }
        // 阻塞，直接休眠当前goroutine，导致死锁崩溃
        gopark(nil, nil, waitReasonChanReceiveNilChan, traceEvGoStop, 2)
        throw("unreachable")
    }

    // Fast path: check for failed non-blocking operation without acquiring the lock.
    // 非阻塞并且协程为空
    if !block && empty(c) {
        // After observing that the channel is not ready for receiving, we observe whether the
        // channel is closed.
        //
        // Reordering of these checks could lead to incorrect behavior when racing with a close.
        // For example, if the channel was open and not empty, was closed, and then drained,
        // reordered reads could incorrectly indicate "open and empty". To prevent reordering,
        // we use atomic loads for both checks, and rely on emptying and closing to happen in
        // separate critical sections under the same lock.  This assumption fails when closing
        // an unbuffered channel with a blocked send, but that is an error condition anyway.
        // 检查channel是否关闭
        if atomic.Load(&c.closed) == 0 {
            // 未关闭，则返回
            // Because a channel cannot be reopened, the later observation of the channel
            // being not closed implies that it was also not closed at the moment of the
            // first observation. We behave as if we observed the channel at that moment
            // and report that the receive cannot proceed.
            return
        }
        // The channel is irreversibly closed. Re-check whether the channel has any pending data
        // to receive, which could have arrived between the empty and closed checks above.
        // Sequential consistency is also required here, when racing with such a send.
        // 未关闭但为空，清空接收的指针ep并返回
        if empty(c) {
            // The channel is irreversibly closed and empty.
            if raceenabled {
                raceacquire(c.raceaddr())
            }
            if ep != nil {
                typedmemclr(c.elemtype, ep)
            }
            return true, false
        }
    }

    var t0 int64
    if blockprofilerate > 0 {
        t0 = cputicks()
    }

    lock(&c.lock)

    if c.closed != 0 {
        // channel 关闭且为空，则清空ep并返回
        if c.qcount == 0 {
            if raceenabled {
                raceacquire(c.raceaddr())
            }
            unlock(&c.lock)
            if ep != nil {
                typedmemclr(c.elemtype, ep)
            }
            return true, false
        }
        // The channel has been closed, but the channel's buffer have data.
    } else {
        // Just found waiting sender with not closed.
        // 有阻塞的发送方，则直接接收数据
        if sg := c.sendq.dequeue(); sg != nil {
            // Found a waiting sender. If buffer is size 0, receive value
            // directly from sender. Otherwise, receive from head of queue
            // and add sender's value to the tail of the queue (both map to
            // the same buffer slot because the queue is full).
            recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
            return true, true
        }
    }

    // 缓冲区有数据，不管channel是否关闭
    if c.qcount > 0 {
        // Receive directly from queue
        // 接收数据，解锁并返回
        qp := chanbuf(c, c.recvx)
        if raceenabled {
            racenotify(c, c.recvx, nil)
        }
        if ep != nil {
            typedmemmove(c.elemtype, ep, qp)
        }
        typedmemclr(c.elemtype, qp)
        c.recvx++
        if c.recvx == c.dataqsiz {
            c.recvx = 0
        }
        c.qcount--
        unlock(&c.lock)
        return true, true
    }

    // 非阻塞，解锁c并返回
    if !block {
        unlock(&c.lock)
        return false, false
    }

    // no sender available: block on this channel.
    // 没有数据可以接收，则阻塞协程
    gp := getg()
    // 获取并初始化sudog
    mysg := acquireSudog()
    mysg.releasetime = 0
    if t0 != 0 {
        mysg.releasetime = -1
    }
    // No stack splits between assigning elem and enqueuing mysg
    // on gp.waiting where copystack can find it.
    mysg.elem = ep
    mysg.waitlink = nil
    gp.waiting = mysg
    mysg.g = gp
    mysg.isSelect = false
    mysg.c = c
    gp.param = nil
    // 加入到接收等待队列
    c.recvq.enqueue(mysg)
    // Signal to anyone trying to shrink our stack that we're about
    // to park on a channel. The window between when this G's status
    // changes and when we set gp.activeStackChans is not safe for
    // stack shrinking.
    // 阻塞当前协程
    gp.parkingOnChan.Store(true)
    gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanReceive, traceEvGoBlockRecv, 2)

    // someone woke us up
    // 被唤醒后，检查sudog的状态
    if mysg != gp.waiting {
        throw("G waiting list is corrupted")
    }
    gp.waiting = nil
    gp.activeStackChans = false
    if mysg.releasetime > 0 {
        blockevent(mysg.releasetime-t0, 2)
    }
    success := mysg.success
    gp.param = nil
    mysg.c = nil
    // 释放sudog
    releaseSudog(mysg)
    return true, success
}

// recv processes a receive operation on a full channel c.
// There are 2 parts:
//  1. The value sent by the sender sg is put into the channel
//     and the sender is woken up to go on its merry way.
//  2. The value received by the receiver (the current G) is
//     written to ep.
//
// For synchronous channels, both values are the same.
// For asynchronous channels, the receiver gets its data from
// the channel buffer and the sender's data is put in the
// channel buffer.
// Channel c must be full and locked. recv unlocks c with unlockf.
// sg must already be dequeued from c.
// A non-nil ep must point to the heap or the caller's stack.
func recv(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
    if c.dataqsiz == 0 {
        // 无缓冲channel的处理
        if raceenabled {
            racesync(c, sg)
        }
        if ep != nil {
            // copy data from sender
            // 接收数据指针不是nil，则直接从发送着处复制数据到ep。
            recvDirect(c.elemtype, sg, ep)
        }
    } else {
        // Queue is full. Take the item at the
        // head of the queue. Make the sender enqueue
        // its item at the tail of the queue. Since the
        // queue is full, those are both the same slot.
        // 有缓冲的channel，进入这个函数表示队列已满
        // 获取队列头部元素的位置
        qp := chanbuf(c, c.recvx)
        if raceenabled {
            racenotify(c, c.recvx, nil)
            racenotify(c, c.recvx, sg)
        }
        // copy data from queue to receiver
        if ep != nil {
            // 将数据从缓冲区复制到接收者
            // 环形队列，取出头部和塞入的尾部在同一个位置
            typedmemmove(c.elemtype, ep, qp)
        }
        // copy data from sender to queue
        // 从发送方拷贝到缓冲队列中
        typedmemmove(c.elemtype, qp, sg.elem)
        // 更新索引
        c.recvx++
        if c.recvx == c.dataqsiz {
            c.recvx = 0
        }
        c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
    }
    // 唤醒发送着
    sg.elem = nil
    gp := sg.g
    unlockf()
    gp.param = unsafe.Pointer(sg)
    // 发送成功
    sg.success = true
    if sg.releasetime != 0 {
        sg.releasetime = cputicks()
    }
    goready(gp, skip+1)
}

// 处理goroutine在channel上阻塞时的辅助函数。
// 主要负责设置相关标志位，确保在堆栈复制或收缩时安全地处理sudog结构
func chanparkcommit(gp *g, chanLock unsafe.Pointer) bool {
    // There are unlocked sudogs that point into gp's stack. Stack
    // copying must lock the channels of those sudogs.
    // Set activeStackChans here instead of before we try parking
    // because we could self-deadlock in stack growth on the
    // channel lock.
    // 这个标志为true时，表示当前goroutine有活跃的channel操作
    // 用于指示在堆栈复制或收缩过程中需要锁定这些sudog所关联的channel
    gp.activeStackChans = true
    // Mark that it's safe for stack shrinking to occur now,
    // because any thread acquiring this G's stack for shrinking
    // is guaranteed to observe activeStackChans after this store.
    // 当这个标志设置为false，表示goroutine不再处于准备阻塞的状态。
    // 这个标志用于确保在堆栈收缩的过程中，任何线程获取这个goroutine的堆栈时都能看到activeStackChans标志
    gp.parkingOnChan.Store(false)
    // Make sure we unlock after setting activeStackChans and
    // unsetting parkingOnChan. The moment we unlock chanLock
    // we risk gp getting readied by a channel operation and
    // so gp could continue running before everything before
    // the unlock is visible (even to gp itself).
    // 解锁
    // 确保在设置前两个标志位之后再解锁，避免竟态条件
    unlock((*mutex)(chanLock))
    return true
}

// compiler implements
//
//	select {
//	case c <- v:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbsend(c, v) {
//		... foo
//	} else {
//		... bar
//	}
func selectnbsend(c *hchan, elem unsafe.Pointer) (selected bool) {
    return chansend(c, elem, false, getcallerpc())
}

// compiler implements
//
//	select {
//	case v, ok = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selected, ok = selectnbrecv(&v, c); selected {
//		... foo
//	} else {
//		... bar
//	}
func selectnbrecv(elem unsafe.Pointer, c *hchan) (selected, received bool) {
    return chanrecv(c, elem, false)
}

//go:linkname reflect_chansend reflect.chansend
func reflect_chansend(c *hchan, elem unsafe.Pointer, nb bool) (selected bool) {
    return chansend(c, elem, !nb, getcallerpc())
}

//go:linkname reflect_chanrecv reflect.chanrecv
func reflect_chanrecv(c *hchan, nb bool, elem unsafe.Pointer) (selected bool, received bool) {
    return chanrecv(c, elem, !nb)
}

//go:linkname reflect_chanlen reflect.chanlen
func reflect_chanlen(c *hchan) int {
    if c == nil {
        return 0
    }
    return int(c.qcount)
}

//go:linkname reflectlite_chanlen internal/reflectlite.chanlen
func reflectlite_chanlen(c *hchan) int {
    if c == nil {
        return 0
    }
    return int(c.qcount)
}

//go:linkname reflect_chancap reflect.chancap
func reflect_chancap(c *hchan) int {
    if c == nil {
        return 0
    }
    return int(c.dataqsiz)
}

//go:linkname reflect_chanclose reflect.chanclose
func reflect_chanclose(c *hchan) {
    closechan(c)
}

func (q *waitq) enqueue(sgp *sudog) {
    sgp.next = nil
    x := q.last
    if x == nil {
        sgp.prev = nil
        q.first = sgp
        q.last = sgp
        return
    }
    sgp.prev = x
    x.next = sgp
    q.last = sgp
}

func (q *waitq) dequeue() *sudog {
    for {
        sgp := q.first
        if sgp == nil {
            return nil
        }
        y := sgp.next
        if y == nil {
            q.first = nil
            q.last = nil
        } else {
            y.prev = nil
            q.first = y
            sgp.next = nil // mark as removed (see dequeueSudoG)
        }

        // if a goroutine was put on this queue because of a
        // select, there is a small window between the goroutine
        // being woken up by a different case and it grabbing the
        // channel locks. Once it has the lock
        // it removes itself from the queue, so we won't see it after that.
        // We use a flag in the G struct to tell us when someone
        // else has won the race to signal this goroutine but the goroutine
        // hasn't removed itself from the queue yet.
        if sgp.isSelect && !sgp.g.selectDone.CompareAndSwap(0, 1) {
            continue
        }

        return sgp
    }
}

func (c *hchan) raceaddr() unsafe.Pointer {
    // Treat read-like and write-like operations on the channel to
    // happen at this address. Avoid using the address of qcount
    // or dataqsiz, because the len() and cap() builtins read
    // those addresses, and we don't want them racing with
    // operations like close().
    return unsafe.Pointer(&c.buf)
}

func racesync(c *hchan, sg *sudog) {
    racerelease(chanbuf(c, 0))
    raceacquireg(sg.g, chanbuf(c, 0))
    racereleaseg(sg.g, chanbuf(c, 0))
    raceacquire(chanbuf(c, 0))
}

// Notify the race detector of a send or receive involving buffer entry idx
// and a channel c or its communicating partner sg.
// This function handles the special case of c.elemsize==0.
func racenotify(c *hchan, idx uint, sg *sudog) {
    // We could have passed the unsafe.Pointer corresponding to entry idx
    // instead of idx itself.  However, in a future version of this function,
    // we can use idx to better handle the case of elemsize==0.
    // A future improvement to the detector is to call TSan with c and idx:
    // this way, Go will continue to not allocating buffer entries for channels
    // of elemsize==0, yet the race detector can be made to handle multiple
    // sync objects underneath the hood (one sync object per idx)
    qp := chanbuf(c, idx)
    // When elemsize==0, we don't allocate a full buffer for the channel.
    // Instead of individual buffer entries, the race detector uses the
    // c.buf as the only buffer entry.  This simplification prevents us from
    // following the memory model's happens-before rules (rules that are
    // implemented in racereleaseacquire).  Instead, we accumulate happens-before
    // information in the synchronization object associated with c.buf.
    if c.elemsize == 0 {
        if sg == nil {
            raceacquire(qp)
            racerelease(qp)
        } else {
            raceacquireg(sg.g, qp)
            racereleaseg(sg.g, qp)
        }
    } else {
        if sg == nil {
            racereleaseacquire(qp)
        } else {
            racereleaseacquireg(sg.g, qp)
        }
    }
}
