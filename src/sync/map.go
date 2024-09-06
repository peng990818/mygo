// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
    "sync/atomic"
)

// Map is like a Go map[interface{}]interface{} but is safe for concurrent use
// by multiple goroutines without additional locking or coordination.
// Loads, stores, and deletes run in amortized constant time.
//
// The Map type is specialized. Most code should use a plain Go map instead,
// with separate locking or coordination, for better type safety and to make it
// easier to maintain other invariants along with the map content.
//
// The Map type is optimized for two common use cases: (1) when the entry for a given
// key is only ever written once but read many times, as in caches that only grow,
// or (2) when multiple goroutines read, write, and overwrite entries for disjoint
// sets of keys. In these two cases, use of a Map may significantly reduce lock
// contention compared to a Go map paired with a separate Mutex or RWMutex.
//
// The zero Map is empty and ready for use. A Map must not be copied after first use.
//
// In the terminology of the Go memory model, Map arranges that a write operation
// “synchronizes before” any read operation that observes the effect of the write, where
// read and write operations are defined as follows.
// Load, LoadAndDelete, LoadOrStore, Swap, CompareAndSwap, and CompareAndDelete
// are read operations; Delete, LoadAndDelete, Store, and Swap are write operations;
// LoadOrStore is a write operation when it returns loaded set to false;
// CompareAndSwap is a write operation when it returns swapped set to true;
// and CompareAndDelete is a write operation when it returns deleted set to true.
// Map 是一种并发安全的 map[interface{}]interface{}，在多个 goroutine 中没有额外的锁条件
// 读取、存储和删除操作的时间复杂度平均为常量
//
// Map 类型非常特殊，大部分代码应该使用原始的 Go map。它具有单独的锁或协调以获得类型安全且更易维护。
//
// Map 类型针对两种常见的用例进行优化：
// 1. 给定 key 只会产生写一次但是却会多次读，类似乎只增的缓存
// 2. 多个 goroutine 读、写以及覆盖不同的 key
// 这两种情况下，与单独使用 Mutex 或 RWMutex 的 map 相比，会显著降低竞争情况
//
// 零值 Map 为空且可以直接使用，Map 使用后不能复制
type Map struct {
    mu Mutex

    // read contains the portion of the map's contents that are safe for
    // concurrent access (with or without mu held).
    //
    // The read field itself is always safe to load, but must only be stored with
    // mu held.
    //
    // Entries stored in read may be updated concurrently without mu, but updating
    // a previously-expunged entry requires that the entry be copied to the dirty
    // map and unexpunged with mu held.
    // read 包含 map 内容的一部分，这些内容对于并发访问是安全的（有或不使用 mu）。
    //
    // read 字段 load 总是安全的，但是必须使用 mu 进行 store。
    //
    // 存储在 read 中的 entry 可以在没有 mu 的情况下并发更新，
    // 但是更新已经删除的 entry 需要将 entry 复制到 dirty map 中，并使用 mu 进行删除。
    read atomic.Pointer[readOnly]

    // dirty contains the portion of the map's contents that require mu to be
    // held. To ensure that the dirty map can be promoted to the read map quickly,
    // it also includes all of the non-expunged entries in the read map.
    //
    // Expunged entries are not stored in the dirty map. An expunged entry in the
    // clean map must be unexpunged and added to the dirty map before a new value
    // can be stored to it.
    //
    // If the dirty map is nil, the next write to the map will initialize it by
    // making a shallow copy of the clean map, omitting stale entries.
    // dirty 含了需要 mu 的 map 内容的一部分。为了确保将 dirty map 快速地转为 read map，
    // 它还包括了 read map 中所有未删除的 entry。
    //
    // 删除的 entry 不会存储在 dirty map 中。在 clean map 中，被删除的 entry 必须被删除并添加到 dirty 中，
    // 然后才能将新的值存储为它
    //
    // 如果 dirty map 为 nil，则下一次的写行为会通过 clean map 的浅拷贝进行初始化
    dirty map[any]*entry

    // misses counts the number of loads since the read map was last updated that
    // needed to lock mu to determine whether the key was present.
    //
    // Once enough misses have occurred to cover the cost of copying the dirty
    // map, the dirty map will be promoted to the read map (in the unamended
    // state) and the next store to the map will make a new dirty copy.
    // misses 计算了从 read map 上一次更新开始的 load 数，需要 lock 以确定 key 是否存在。
    //
    // 一旦发生足够的 misses 足以囊括复制 dirty map 的成本，dirty map 将被提升为 read map（处于未修改状态）
    // 并且 map 的下一次 store 将生成新的 dirty 副本。
    misses int
}

// readOnly is an immutable struct stored atomically in the Map.read field.
type readOnly struct {
    m map[any]*entry
    // 如果脏映射包含一些不在 m 中的键，则为 true。
    amended bool // true if the dirty map contains some key not in m.
}

// expunged is an arbitrary pointer that marks entries which have been deleted
// from the dirty map.
// expunged 是一个任意指针，用于标记已从脏映射中删除的条目。
var expunged = new(any)

// An entry is a slot in the map corresponding to a particular key.
// 只是简单的创建一个 entry
// entry 是一个对应于 map 中特殊 key 的 slot
type entry struct {
    // p points to the interface{} value stored for the entry.
    //
    // If p == nil, the entry has been deleted, and either m.dirty == nil or
    // m.dirty[key] is e.
    //
    // If p == expunged, the entry has been deleted, m.dirty != nil, and the entry
    // is missing from m.dirty.
    //
    // Otherwise, the entry is valid and recorded in m.read.m[key] and, if m.dirty
    // != nil, in m.dirty[key].
    //
    // An entry can be deleted by atomic replacement with nil: when m.dirty is
    // next created, it will atomically replace nil with expunged and leave
    // m.dirty[key] unset.
    //
    // An entry's associated value can be updated by atomic replacement, provided
    // p != expunged. If p == expunged, an entry's associated value can be updated
    // only after first setting m.dirty[key] = e so that lookups using the dirty
    // map find the entry.
    // p 指向 interface{} 类型的值，用于保存 entry
    //
    // 如果 p == nil，则 entry 已被删除，且 m.dirty == nil
    //
    // 如果 p == expunged, 则 entry 已经被删除，m.dirty != nil ，则 entry 不在 m.dirty 中
    //
    // 否则，entry 仍然有效，且被记录在 m.read.m[key] ，但如果 m.dirty != nil，则在 m.dirty[key] 中
    //
    // 一个 entry 可以被原子替换为 nil 来删除：当 m.dirty 下一次创建时，它会自动将 nil 替换为 expunged 且
    // 让 m.dirty[key] 成为未设置的状态。
    //
    // 与一个 entry 关联的值可以被原子替换式的更新，提供的 p != expunged。如果 p == expunged，
    // 则与 entry 关联的值只能在 m.dirty[key] = e 设置后被更新，因此会使用 dirty map 来查找 entry。
    p atomic.Pointer[any]
}

func newEntry(i any) *entry {
    e := &entry{}
    e.p.Store(&i)
    return e
}

func (m *Map) loadReadOnly() readOnly {
    if p := m.read.Load(); p != nil {
        return *p
    }
    return readOnly{}
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
// Load 返回了存储在 map 中对应于 key 的值 value，如果不存在则返回 nil
// ok 表示了值能否在 map 中找到
func (m *Map) Load(key any) (value any, ok bool) {
    // 获取read map
    read := m.loadReadOnly()
    // 读取对应的值
    e, ok := read.m[key]
    // 如果在 read map 中找不到，且 dirty map 包含 read map 中不存在的 key，则进一步查找
    if !ok && read.amended {
        m.mu.Lock()
        // Avoid reporting a spurious miss if m.dirty got promoted while we were
        // blocked on m.mu. (If further loads of the same key will not miss, it's
        // not worth copying the dirty map for this key.)
        // 再一次获取，双重检查
        read = m.loadReadOnly()
        e, ok = read.m[key]
        // 如果这时 read map 确实读不到，且 dirty map 与 read map 不一致
        if !ok && read.amended {
            // 从dirty map中读取
            e, ok = m.dirty[key]
            // Regardless of whether the entry was present, record a miss: this key
            // will take the slow path until the dirty map is promoted to the read
            // map.
            // 无论 entry 是否找到，记录一次 miss：该 key 会采取 slow path 进行读取，直到
            // dirty map 被提升为 read map。
            m.missLocked()
        }
        m.mu.Unlock()
    }
    // 如果 read map 或者 dirty map 中找不到 key，则确实没找到，返回 nil 和 false
    if !ok {
        return nil, false
    }
    // 找到了，返回读到的值
    return e.load()
}

func (e *entry) load() (value any, ok bool) {
    // 读entry的值
    p := e.p.Load()
    // 判断是否被删除
    if p == nil || p == expunged {
        return nil, false
    }
    // 读取值
    return *p, true
}

// Store sets the value for a key.
func (m *Map) Store(key, value any) {
    _, _ = m.Swap(key, value)
}

// tryCompareAndSwap compare the entry with the given old value and swaps
// it with a new value if the entry is equal to the old value, and the entry
// has not been expunged.
//
// If the entry is expunged, tryCompareAndSwap returns false and leaves
// the entry unchanged.
func (e *entry) tryCompareAndSwap(old, new any) bool {
    p := e.p.Load()
    if p == nil || p == expunged || *p != old {
        return false
    }

    // Copy the interface after the first load to make this method more amenable
    // to escape analysis: if the comparison fails from the start, we shouldn't
    // bother heap-allocating an interface value to store.
    nc := new
    for {
        if e.p.CompareAndSwap(p, &nc) {
            return true
        }
        p = e.p.Load()
        if p == nil || p == expunged || *p != old {
            return false
        }
    }
}

// unexpungeLocked ensures that the entry is not marked as expunged.
//
// If the entry was previously expunged, it must be added to the dirty map
// before m.mu is unlocked.
// unexpungeLocked 确保条目不会被标记为已删除。如果该条目之前已被删除，则必须在 m.mu 解锁之前将其添加到脏映射中。
func (e *entry) unexpungeLocked() (wasExpunged bool) {
    return e.p.CompareAndSwap(expunged, nil)
}

// swapLocked unconditionally swaps a value into the entry.
//
// The entry must be known not to be expunged.
func (e *entry) swapLocked(i *any) *any {
    return e.p.Swap(i)
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
// LoadOrStore 在 key 已经存在时，返回存在的值，否则存储当前给定的值
// loaded 为 true 表示 actual 读取成功，否则为 false 表示 value 存储成功
func (m *Map) LoadOrStore(key, value any) (actual any, loaded bool) {
    // Avoid locking if it's a clean hit.
    read := m.loadReadOnly()
    // 如果 read map 中已经读到
    if e, ok := read.m[key]; ok {
        // 尝试存储（可能 key 是一个已删除的 key）
        actual, loaded, ok := e.tryLoadOrStore(value)
        // 如果存储成功，则直接返回
        if ok {
            return actual, loaded
        }
    }

    // 否则，涉及 dirty map，加锁
    m.mu.Lock()
    // 再读一次 read map
    read = m.loadReadOnly()
    if e, ok := read.m[key]; ok {
        // 如果 read map 中已经读到，则看该值是否被删除
        if e.unexpungeLocked() {
            // 没有被删除，则通过 dirty map 存
            m.dirty[key] = e
        }
        actual, loaded, _ = e.tryLoadOrStore(value)
    } else if e, ok := m.dirty[key]; ok {
        // 如果 read map 没找到, dirty map 找到了
        // 尝试 laod or store，并记录 miss
        actual, loaded, _ = e.tryLoadOrStore(value)
        m.missLocked()
    } else {
        // 否则就是存一个新的值
        // 如果 read map 和 dirty map 相同，则开始标记不同
        if !read.amended {
            // We're adding the first new key to the dirty map.
            // Make sure it is allocated and mark the read-only map as incomplete.
            m.dirtyLocked()
            m.read.Store(&readOnly{m: read.m, amended: true})
        }
        // 存到 dirty map 中去
        m.dirty[key] = newEntry(value)
        actual, loaded = value, false
    }
    m.mu.Unlock()

    return actual, loaded
}

// tryLoadOrStore atomically loads or stores a value if the entry is not
// expunged.
//
// If the entry is expunged, tryLoadOrStore leaves the entry unchanged and
// returns with ok==false.
func (e *entry) tryLoadOrStore(i any) (actual any, loaded, ok bool) {
    p := e.p.Load()
    if p == expunged {
        return nil, false, false
    }
    if p != nil {
        return *p, true, true
    }

    // Copy the interface after the first load to make this method more amenable
    // to escape analysis: if we hit the "load" path or the entry is expunged, we
    // shouldn't bother heap-allocating.
    ic := i
    for {
        if e.p.CompareAndSwap(nil, &ic) {
            return i, false, true
        }
        p = e.p.Load()
        if p == expunged {
            return nil, false, false
        }
        if p != nil {
            return *p, true, true
        }
    }
}

// LoadAndDelete deletes the value for a key, returning the previous value if any.
// The loaded result reports whether the key was present.
func (m *Map) LoadAndDelete(key any) (value any, loaded bool) {
    // 获得 read map
    read := m.loadReadOnly()
    // 从 read map 中读取需要删除的 key
    e, ok := read.m[key]
    // 如果 read map 中没找到，且 read map 与 dirty map 不一致
    // 说明要删除的值在 dirty map 中
    if !ok && read.amended {
        m.mu.Lock()
        // 再次读 read map
        read = m.loadReadOnly()
        // 从 read map 中取值
        e, ok = read.m[key]
        // 没取到，read map 和 dirty map 不一致
        if !ok && read.amended {
            e, ok = m.dirty[key]
            // 删除 dierty map 的值
            delete(m.dirty, key)
            // Regardless of whether the entry was present, record a miss: this key
            // will take the slow path until the dirty map is promoted to the read
            // map.
            // 记录一次miss
            m.missLocked()
        }
        m.mu.Unlock()
    }
    if ok {
        return e.delete()
    }
    return nil, false
}

// Delete deletes the value for a key.
// Delete 删除 key 对应的 value
func (m *Map) Delete(key any) {
    m.LoadAndDelete(key)
}

func (e *entry) delete() (value any, ok bool) {
    for {
        // 读取 entry 的值
        p := e.p.Load()
        // 如果 p 等于 nil，或者 p 已经标记删除
        if p == nil || p == expunged {
            // 则不需要删除
            return nil, false
        }
        // 否则，将 p 的值与 nil 进行原子换
        if e.p.CompareAndSwap(p, nil) {
            // 删除成功（本质只是解除引用，实际上是留给 GC 清理）
            return *p, true
        }
    }
}

// trySwap swaps a value if the entry has not been expunged.
//
// If the entry is expunged, trySwap returns false and leaves the entry
// unchanged.
// 如果条目尚未删除，trySwap 会交换一个值。如果该条目被删除，trySwap 将返回 false 并保持该条目不变。
func (e *entry) trySwap(i *any) (*any, bool) {
    for {
        // 读取entry
        p := e.p.Load()
        // 如果entry已经删除则无法存储
        if p == expunged {
            return nil, false
        }
        // 交换p和i的值，成功则立即返回
        if e.p.CompareAndSwap(p, i) {
            return p, true
        }
    }
}

// Swap swaps the value for a key and returns the previous value if any.
// The loaded result reports whether the key was present.
func (m *Map) Swap(key, value any) (previous any, loaded bool) {
    read := m.loadReadOnly()
    if e, ok := read.m[key]; ok {
        if v, ok := e.trySwap(&value); ok {
            if v == nil {
                return nil, false
            }
            return *v, true
        }
    }

    m.mu.Lock()
    // read map可能已经更新，所以重新加载一下
    read = m.loadReadOnly()
    if e, ok := read.m[key]; ok {
        // 修改一个已经存在的值
        if e.unexpungeLocked() {
            // The entry was previously expunged, which implies that there is a
            // non-nil dirty map and this entry is not in it.
            // 说明 entry 先前是被标记为删除了的，现在我们又要存储它，只能向 dirty map 进行更新了
            m.dirty[key] = e
        }
        // 无论先前删除与否，均要更新read map
        if v := e.swapLocked(&value); v != nil {
            loaded = true
            previous = *v
        }
    } else if e, ok := m.dirty[key]; ok {
        // 更新dirty map的值
        if v := e.swapLocked(&value); v != nil {
            loaded = true
            previous = *v
        }
    } else {
        // 如果dirty map里没有read map没有的值
        if !read.amended {
            // We're adding the first new key to the dirty map.
            // Make sure it is allocated and mark the read-only map as incomplete.
            // 首次添加一个新值到dirty map中
            // 确保已被分配并标记为read map是不完备的，即dirty map有，read map没有
            m.dirtyLocked()
            // 更新amended， 标记 read map 中缺少了值（标记为两者不同）
            m.read.Store(&readOnly{m: read.m, amended: true})
        }
        // 不管 read map 和 dirty map 相同与否，正式保存新的值
        m.dirty[key] = newEntry(value)
    }
    m.mu.Unlock()
    return previous, loaded
}

// CompareAndSwap swaps the old and new values for key
// if the value stored in the map is equal to old.
// The old value must be of a comparable type.
func (m *Map) CompareAndSwap(key, old, new any) bool {
    read := m.loadReadOnly()
    if e, ok := read.m[key]; ok {
        return e.tryCompareAndSwap(old, new)
    } else if !read.amended {
        return false // No existing value for key.
    }

    m.mu.Lock()
    defer m.mu.Unlock()
    read = m.loadReadOnly()
    swapped := false
    if e, ok := read.m[key]; ok {
        swapped = e.tryCompareAndSwap(old, new)
    } else if e, ok := m.dirty[key]; ok {
        swapped = e.tryCompareAndSwap(old, new)
        // We needed to lock mu in order to load the entry for key,
        // and the operation didn't change the set of keys in the map
        // (so it would be made more efficient by promoting the dirty
        // map to read-only).
        // Count it as a miss so that we will eventually switch to the
        // more efficient steady state.
        m.missLocked()
    }
    return swapped
}

// CompareAndDelete deletes the entry for key if its value is equal to old.
// The old value must be of a comparable type.
//
// If there is no current value for key in the map, CompareAndDelete
// returns false (even if the old value is the nil interface value).
func (m *Map) CompareAndDelete(key, old any) (deleted bool) {
    read := m.loadReadOnly()
    e, ok := read.m[key]
    if !ok && read.amended {
        m.mu.Lock()
        read = m.loadReadOnly()
        e, ok = read.m[key]
        if !ok && read.amended {
            e, ok = m.dirty[key]
            // Don't delete key from m.dirty: we still need to do the “compare” part
            // of the operation. The entry will eventually be expunged when the
            // dirty map is promoted to the read map.
            //
            // Regardless of whether the entry was present, record a miss: this key
            // will take the slow path until the dirty map is promoted to the read
            // map.
            m.missLocked()
        }
        m.mu.Unlock()
    }
    for ok {
        p := e.p.Load()
        if p == nil || p == expunged || *p != old {
            return false
        }
        if e.p.CompareAndSwap(p, nil) {
            return true
        }
    }
    return false
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently (including by f), Range may reflect any
// mapping for that key from any point during the Range call. Range does not
// block other methods on the receiver; even f itself may call any method on m.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
// Range 为每个 key 顺序的调用 f。如果 f 返回 false，则 range 会停止迭代。
//
// Range 的时间复杂度可能会是 O(N) 即便是 f 返回 false。
func (m *Map) Range(f func(key, value any) bool) {
    // We need to be able to iterate over all of the keys that were already
    // present at the start of the call to Range.
    // If read.amended is false, then read.m satisfies that property without
    // requiring us to hold m.mu for a long time.
    // 读取 read map
    read := m.loadReadOnly()
    // 如果 read map 和 dirty map 不一致，则需要进一步操作
    if read.amended {
        // m.dirty contains keys not in read.m. Fortunately, Range is already O(N)
        // (assuming the caller does not break out early), so a call to Range
        // amortizes an entire copy of the map: we can promote the dirty copy
        // immediately!
        m.mu.Lock()
        // 再读一次，如果还是不一致，则将 dirty map 提升为 read map
        read = m.loadReadOnly()
        if read.amended {
            read = readOnly{m: m.dirty}
            m.read.Store(&read)
            m.dirty = nil
            m.misses = 0
        }
        m.mu.Unlock()
    }

    // 在 read 变量中读（可能是 read map ，也可能是 dirty map 同步过来的 map）
    for k, e := range read.m {
        // 读 readOnly，load 会检查该值是否被标记为删除
        v, ok := e.load()
        // 如果已经删除，则跳过
        if !ok {
            continue
        }
        // 如果 f 返回 false，则停止迭代
        if !f(k, v) {
            break
        }
    }
}

// 此方法调用时，整个 map 是锁住的
func (m *Map) missLocked() {
    // 增加miss数量
    m.misses++
    // 如果 miss 的次数小于 dirty map 的 key 数
    // 则直接返回
    if m.misses < len(m.dirty) {
        return
    }
    // 否则将 dirty map 同步到 read map 去
    m.read.Store(&readOnly{m: m.dirty})
    // 清空 dirty map
    m.dirty = nil
    // miss 计数归零
    m.misses = 0
}

func (m *Map) dirtyLocked() {
    if m.dirty != nil {
        return
    }

    read := m.loadReadOnly()
    m.dirty = make(map[any]*entry, len(read.m))
    for k, e := range read.m {
        // 检查当前条目是否过期或失效
        if !e.tryExpungeLocked() {
            m.dirty[k] = e
        }
    }
}

func (e *entry) tryExpungeLocked() (isExpunged bool) {
    // 获取entry的值
    p := e.p.Load()
    // 如果entry的值为nil
    for p == nil {
        // 检查是否被标记为已经删除
        if e.p.CompareAndSwap(nil, expunged) {
            // 成功交换，说明已经被删除
            return true
        }
        // 删除操作失败，说明expunged等于nil，则重新读取
        p = e.p.Load()
    }
    return p == expunged
}
