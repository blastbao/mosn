/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package shm

import (
	"errors"
	"reflect"
	"strconv"
	"unsafe"
)

var (
	hashEntrySize = int(unsafe.Sizeof(hashEntry{}))
	hashMetaSize  = int(unsafe.Sizeof(meta{}))
)

// hash
const (
	// offset32 FNVa offset basis. See https://en.wikipedia.org/wiki/Fowler–Noll–Vo_hash_function#FNV-1a_hash
	offset32 = 2166136261
	// prime32 FNVa prime value. See https://en.wikipedia.org/wiki/Fowler–Noll–Vo_hash_function#FNV-1a_hash
	prime32 = 16777619

	// indicate end of linked-list
	sentinel = 0xffffffff
)

// gets the string and returns its uint32 hash value.
func hash(key string) uint32 {
	var hash uint32 = offset32
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= prime32
	}

	return hash
}

type hashSet struct {
	entry []hashEntry	// 占用 meta.cap * sizeof(hashEntry) 的大小
	meta  *meta			//
	slots []uint32		// 占用 meta.slotsNum * 4 的大小
}

type hashEntry struct {
	metricsEntry	// <name, value, ref> , 120B
	next uint32		// 4B

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	//
	// 填满 128 B
	pad [128 - unsafe.Sizeof(metricsEntry{})%128 - 4]byte
}

type meta struct {
	cap       uint32	// 容量
	size      uint32	// 元素数
	freeIndex uint32	// 下一个可用的空闲 entry ，通过 next 指针将所有空闲 entry 串联起来，类似于空闲对象池。

	slotsNum uint32		// 假设 slotsNum == 128 ，那么所有 <k, v> 都 hash 到这 128 个桶，每个桶内是一个链表，链表元素即 `hashEntry` 。
	bytesNum uint32
}


// 大致说一下哈希表初始化的算法：
//
// 首先 alignedSize 表示 4k 对齐后的 ShmSpan 大小，
// 前 8 个字节被分配为互斥锁和引用计数，
// 另外 20 个字节被分配为哈希表的 meta 结构体，

func newHashSet(segment uintptr, bytesNum, cap, slotsNum int, init bool) (*hashSet, error) {
	set := &hashSet{}

	// 1. entry mapping
	entrySlice := (*reflect.SliceHeader)(unsafe.Pointer(&set.entry))
	entrySlice.Data = segment
	entrySlice.Len = cap
	entrySlice.Cap = cap

	offset := cap * hashEntrySize
	if offset > bytesNum {
		return nil, errors.New("segment is not enough to map hashSet.entry")
	}

	// 2. meta mapping
	set.meta = (*meta)(unsafe.Pointer(segment + uintptr(offset)))
	set.meta.slotsNum = uint32(slotsNum)
	set.meta.bytesNum = uint32(bytesNum)
	set.meta.cap = uint32(cap)

	offset += hashMetaSize
	if offset > bytesNum {
		return nil, errors.New("segment is not enough to map hashSet.meta")
	}

	// 3. slots mapping
	slotSlice := (*reflect.SliceHeader)(unsafe.Pointer(&set.slots))
	slotSlice.Data = segment + uintptr(offset)
	slotSlice.Len = slotsNum
	slotSlice.Cap = slotsNum

	offset += 4 * slotsNum // slot type is uint32
	if offset > bytesNum {
		return nil, errors.New("segment is not enough to map hashSet.slots")
	}

	// 初始化
	if init {
		// 4. initialize
		// 4.1 meta
		set.meta.size = 0
		set.meta.freeIndex = 0

		// 4.2 slots
		for i := 0; i < slotsNum; i++ {
			set.slots[i] = sentinel
		}

		// 4.3 entries
		last := cap - 1
		for i := 0; i < last; i++ {
			set.entry[i].next = uint32(i + 1)
		}
		set.entry[last].next = sentinel
	}
	return set, nil
}

func (s *hashSet) Alloc(name string) (*hashEntry, bool) {
	// 1. search existed slots and entries
	// 计算 hash 值作为 slot index
	h := hash(name)
	slot := h % s.meta.slotsNum

	// name convert if length exceeded
	if len(name) > maxNameLength {
		// if name is longer than max length, use hash_string as leading character
		// and the remaining maxNameLength - len(hash_string) bytes follows
		hStr := strconv.Itoa(int(h))
		name = hStr + name[len(hStr)+len(name)-maxNameLength:]
	}

	nameBytes := []byte(name)

	// 定位到 slot ，查找对应的 entry
	var entry *hashEntry
	for index := s.slots[slot]; index != sentinel; {
		entry = &s.entry[index]
		if entry.equalName(nameBytes) {
			return entry, false
		}
		index = entry.next
	}

	// 2. create new entry
	// 如果找不到, 创建新的 entry

	// 超过限制，报错
	if s.meta.size >= s.meta.cap {
		return nil, false
	}

	// 创建新的 entry
	newIndex := s.meta.freeIndex		// 取一个空闲 entry
	newEntry := &s.entry[newIndex]
	newEntry.assignName(nameBytes)		// 设置 name
	newEntry.ref = 1					// 设置 ref

	// 将 entry 插入到 slot 中
	if entry == nil {
		s.slots[slot] = newIndex 	// 初始化 slot 链表
	} else {
		entry.next = newIndex 		// 插入到 slot 链表尾
	}

	// 更新元数据
	s.meta.size++ 						// 元素数
	s.meta.freeIndex = newEntry.next 	// 占用当前 entry ，下次分配从 next 开始
	newEntry.next = sentinel

	return newEntry, true
}

func (s *hashSet) Free(entry *hashEntry) {
	if entry.decRef() {
		name := string(entry.getName())

		// 1. search existed slots and entries
		h := hash(name)
		slot := h % s.meta.slotsNum

		var index uint32
		var prev *hashEntry
		for index = s.slots[slot]; index != sentinel; {
			target := &s.entry[index]
			if entry == target {
				break
			}

			prev = target
			index = target.next
		}

		// 2. unlink, re-init and add to the head of free list
		if prev != nil {
			prev.next = entry.next
		} else {
			s.slots[slot] = entry.next
		}

		*entry = hashEntry{}

		entry.next = s.meta.freeIndex
		s.meta.freeIndex = index
	}
}
