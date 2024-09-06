// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.staticlockranking

package sync

import "unsafe"

// Approximation of notifyList in runtime/sema.go. Size and alignment must
// agree.
type notifyList struct {
    // wait为下一个等待者的ticket编号
    // 在没有lock的情况下原子自增
    wait uint32
    // notify是下一个被通知的等待者的ticket编号
    // 可以在没有lock的情况下进行读取。但只有在持有lock的情况下才能写
    notify uint32
    // 等待者列表
    lock uintptr // key field of the mutex
    head unsafe.Pointer
    tail unsafe.Pointer
}
