// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || 386

package runtime

import (
    "internal/goarch"
    "unsafe"
)

// adjust Gobuf as if it executed a call to fn with context ctxt
// and then stopped before the first instruction in fn.
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
    // 栈指针调整
    sp := buf.sp
    // 准备函数的返回地址并把它放在栈上
    sp -= goarch.PtrSize
    // 将调用者的 pc 保存到栈中，这样当新函数执行完毕时，可以通过这个返回地址恢复执行原来的函数。
    *(*uintptr)(unsafe.Pointer(sp)) = buf.pc
    // 调整 Gobuf 的栈指针和程序计数器以指向函数fn
    // 准备好函数执行时的栈帧
    buf.sp = sp
    // 将 Goroutine 的 pc 设置为将要执行的函数的入口地址。这样，当 Goroutine 被调度运行时，它会从这个函数的入口地址开始执行。
    buf.pc = uintptr(fn)
    // 保存了funcval，即函数及其参数的引用
    buf.ctxt = ctxt
}
