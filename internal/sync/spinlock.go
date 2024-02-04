// Copyright 2019 Andy Pan & Dietoad. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package sync

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// spinlock 本质就是一个uint32的正整数
type spinLock uint32

const maxBackoff = 16

func (sl *spinLock) Lock() {
	backoff := 1
	for !atomic.CompareAndSwapUint32((*uint32)(sl), 0, 1) { // false -> 表示当前的sl的值【不是0】
		// Leverage the exponential backoff algorithm, see https://en.wikipedia.org/wiki/Exponential_backoff.
		for i := 0; i < backoff; i++ {
			runtime.Gosched() // 这个的意思就是暂时让出时间片 1次 2次 4次 8次 16次，backoff 数值越大，让出的时间片的次数也就越多
		}
		if backoff < maxBackoff {
			backoff <<= 1
		}
	}
}

func (sl *spinLock) Unlock() {
	atomic.StoreUint32((*uint32)(sl), 0) // 修改为0
}

// NewSpinLock instantiates a spin-lock.
func NewSpinLock() sync.Locker { // 自旋锁
	return new(spinLock)
}
