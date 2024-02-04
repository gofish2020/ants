// MIT License

// Copyright (c) 2018 Andy Pan

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package ants

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	syncx "github.com/panjf2000/ants/v2/internal/sync"
)

// Pool accepts the tasks and process them concurrently,
// it limits the total of goroutines to a given number by recycling goroutines.
type Pool struct {
	// capacity of the pool, a negative value means that the capacity of pool is limitless, an infinite pool is used to
	// avoid potential issue of endless blocking caused by nested usage of a pool: submitting a task to pool
	// which submits a new task to the same pool.
	capacity int32

	// running is the number of the currently running goroutines.
	running int32

	// lock for protecting the worker queue.
	lock sync.Locker

	// workers is a slice that store the available workers.
	workers workerQueue // 这里缓存协程对象

	// state is used to notice the pool to closed itself.
	state int32

	// cond for waiting to get an idle worker.
	cond *sync.Cond

	// workerCache speeds up the obtainment of a usable worker in function:retrieveWorker.
	workerCache sync.Pool // 生成goWorker对象（就是生成协程对象）

	// waiting is the number of goroutines already been blocked on pool.Submit(), protected by pool.lock
	waiting int32

	purgeDone int32
	stopPurge context.CancelFunc

	ticktockDone int32
	stopTicktock context.CancelFunc

	now atomic.Value

	options *Options
}

// purgeStaleWorkers clears stale workers periodically, it runs in an individual goroutine, as a scavenger.
func (p *Pool) purgeStaleWorkers(ctx context.Context) {
	ticker := time.NewTicker(p.options.ExpiryDuration)

	defer func() {
		ticker.Stop()
		atomic.StoreInt32(&p.purgeDone, 1)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C: // 每过1s执行一次
		}

		// 关闭直接结束，清理协程
		if p.IsClosed() {
			break
		}

		var isDormant bool
		p.lock.Lock()                                               // 协程对象池，操作前，需要加锁
		staleWorkers := p.workers.refresh(p.options.ExpiryDuration) // 就是剔除 空闲过久的 *goWorker
		n := p.Running()
		isDormant = n == 0 || n == len(staleWorkers) // 全部清理（一个不剩 or 本身存活的 *goWorker就是空）
		p.lock.Unlock()

		// Notify obsolete workers to stop.
		// This notification must be outside the p.lock, since w.task
		// may be blocking and may consume a lot of time if many workers
		// are located on non-local CPUs.
		for i := range staleWorkers {
			staleWorkers[i].finish() // 【让这些协程对象停止运行，并且回收起来】
			staleWorkers[i] = nil
		}

		// There might be a situation where all workers have been cleaned up (no worker is running),
		// while some invokers still are stuck in p.cond.Wait(), then we need to awake those invokers.
		if isDormant && p.Waiting() > 0 { // 协程对象全部被清理，并且如果还有任务等待执行，让他直接清醒过来
			p.cond.Broadcast()
		}
	}
}

// ticktock is a goroutine that updates the current time in the pool regularly.
func (p *Pool) ticktock(ctx context.Context) {
	ticker := time.NewTicker(nowTimeUpdateInterval) // 500ms
	defer func() {
		ticker.Stop()
		atomic.StoreInt32(&p.ticktockDone, 1)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if p.IsClosed() {
			break
		}

		p.now.Store(time.Now())
	}
}

func (p *Pool) goPurge() {
	if p.options.DisablePurge { // 禁用清理
		return
	}

	// Start a goroutine to clean up expired workers periodically.
	var ctx context.Context
	ctx, p.stopPurge = context.WithCancel(context.Background())
	go p.purgeStaleWorkers(ctx)
}

func (p *Pool) goTicktock() {
	p.now.Store(time.Now())
	var ctx context.Context
	ctx, p.stopTicktock = context.WithCancel(context.Background())
	go p.ticktock(ctx)
}

func (p *Pool) nowTime() time.Time {
	return p.now.Load().(time.Time)
}

// NewPool instantiates a Pool with customized options.
func NewPool(size int, options ...Option) (*Pool, error) {
	if size <= 0 {
		size = -1
	}

	// 加载配置
	opts := loadOptions(options...)

	if !opts.DisablePurge { // false （允许清除）
		if expiry := opts.ExpiryDuration; expiry < 0 {
			return nil, ErrInvalidPoolExpiry
		} else if expiry == 0 {
			opts.ExpiryDuration = DefaultCleanIntervalTime // 默认【空闲1s就清理】
		}
	}

	// 日志器
	if opts.Logger == nil {
		opts.Logger = defaultLogger
	}

	p := &Pool{
		capacity: int32(size),         // 协程对象的个数
		lock:     syncx.NewSpinLock(), // 自旋锁（就是互斥锁而已）
		options:  opts,                // 配置
	}

	// 协程对象（用于创建协程对象）
	p.workerCache.New = func() interface{} {
		return &goWorker{
			pool: p,
			task: make(chan func(), workerChanCap),
		}
	}

	if p.options.PreAlloc { // 预分配 协程对象
		if size == -1 {
			return nil, ErrInvalidPreAllocSize
		}
		p.workers = newWorkerQueue(queueTypeLoopQueue, size) // 循环队列
	} else {
		p.workers = newWorkerQueue(queueTypeStack, 0) // 栈
	}

	p.cond = sync.NewCond(p.lock)

	// 清理长时间没有使用的 *goWorker
	p.goPurge()
	p.goTicktock()

	return p, nil
}

// Submit submits a task to this pool.
//
// Note that you are allowed to call Pool.Submit() from the current Pool.Submit(),
// but what calls for special attention is that you will get blocked with the last
// Pool.Submit() call once the current Pool runs out of its capacity, and to avoid this,
// you should instantiate a Pool with ants.WithNonblocking(true).
func (p *Pool) Submit(task func()) error {
	if p.IsClosed() {
		return ErrPoolClosed
	}

	// 获取一个协程对象（处理task）
	w, err := p.retrieveWorker()
	if w != nil {
		w.inputFunc(task)
	}
	return err
}

// Running returns the number of workers currently running.
func (p *Pool) Running() int {
	return int(atomic.LoadInt32(&p.running))
}

// Free returns the number of available workers, -1 indicates this pool is unlimited.
func (p *Pool) Free() int { // 表示：还可以启动多少个协程对象；
	c := p.Cap()
	if c < 0 {
		return -1
	}
	return c - p.Running()
}

// Waiting returns the number of tasks waiting to be executed.
func (p *Pool) Waiting() int {
	return int(atomic.LoadInt32(&p.waiting))
}

// Cap returns the capacity of this pool.
func (p *Pool) Cap() int {
	return int(atomic.LoadInt32(&p.capacity))
}

// Tune changes the capacity of this pool, note that it is noneffective to the infinite or pre-allocation pool.
func (p *Pool) Tune(size int) { // 动态调整容量
	capacity := p.Cap() // 当前的容量
	if capacity == -1 || size <= 0 || size == capacity || p.options.PreAlloc {
		return
	}
	atomic.StoreInt32(&p.capacity, int32(size))
	if size > capacity {
		if size-capacity == 1 {
			p.cond.Signal()
			return
		}
		p.cond.Broadcast()
	}
}

// IsClosed indicates whether the pool is closed.
func (p *Pool) IsClosed() bool {
	return atomic.LoadInt32(&p.state) == CLOSED
}

// Release closes this pool and releases the worker queue.
func (p *Pool) Release() {
	if !atomic.CompareAndSwapInt32(&p.state, OPENED, CLOSED) {
		return
	}

	if p.stopPurge != nil { // 停止 清理协程
		p.stopPurge()
		p.stopPurge = nil
	}
	p.stopTicktock() // 停止 当前时间更新协程
	p.stopTicktock = nil

	p.lock.Lock()
	p.workers.reset()
	p.lock.Unlock()
	// There might be some callers waiting in retrieveWorker(), so we need to wake them up to prevent
	// those callers blocking infinitely.
	p.cond.Broadcast() // 唤醒阻塞在 *Pool对象上的协程
}

// ReleaseTimeout is like Release but with a timeout, it waits all workers to exit before timing out.
func (p *Pool) ReleaseTimeout(timeout time.Duration) error {
	if p.IsClosed() || (!p.options.DisablePurge && p.stopPurge == nil) || p.stopTicktock == nil {
		return ErrPoolClosed
	}
	p.Release()

	endTime := time.Now().Add(timeout)
	for time.Now().Before(endTime) {

		// 检查：运行中的协程对象个数 ，清理协程停止， 时间更新协程停止
		if p.Running() == 0 &&
			(p.options.DisablePurge || atomic.LoadInt32(&p.purgeDone) == 1) &&
			atomic.LoadInt32(&p.ticktockDone) == 1 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ErrTimeout
}

// Reboot reboots a closed pool.
func (p *Pool) Reboot() {
	if atomic.CompareAndSwapInt32(&p.state, CLOSED, OPENED) {
		atomic.StoreInt32(&p.purgeDone, 0)
		p.goPurge()
		atomic.StoreInt32(&p.ticktockDone, 0)
		p.goTicktock()
	}
}

func (p *Pool) addRunning(delta int) {

	atomic.AddInt32(&p.running, int32(delta)) // 记录正在运行的协程数量
}

func (p *Pool) addWaiting(delta int) {
	atomic.AddInt32(&p.waiting, int32(delta))
}

// retrieveWorker returns an available worker to run the tasks.
func (p *Pool) retrieveWorker() (w worker, err error) {
	p.lock.Lock()

retry:
	// 从协程池中，获取一个协程对象
	if w = p.workers.detach(); w != nil {
		p.lock.Unlock()
		return
	}

	if capacity := p.Cap(); capacity == -1 || capacity > p.Running() { // capacity 容量-1表示无限  or  容量还没有满
		p.lock.Unlock()
		w = p.workerCache.Get().(*goWorker) // 创建一个协程对象
		w.run()                             // 并启动协程（等待任务中...)
		return
	}

	// 执行到这里，说明没有获取到协程对象（表示没有空闲的协程对象使用）
	// 如果非阻塞，直接返回
	// 如果是阻塞，但是阻塞的太多了，也直接返回

	if p.options.Nonblocking || (p.options.MaxBlockingTasks != 0 && p.Waiting() >= p.options.MaxBlockingTasks) {
		p.lock.Unlock()
		return nil, ErrPoolOverload
	}

	// 否则，直接阻塞，等待有 *goWorker 空闲了，再去抢一个
	p.addWaiting(1)
	p.cond.Wait() // block and wait for an available worker
	p.addWaiting(-1)

	if p.IsClosed() {
		p.lock.Unlock()
		return nil, ErrPoolClosed
	}

	goto retry
}

// revertWorker puts a worker back into free pool, recycling the goroutines.
func (p *Pool) revertWorker(worker *goWorker) bool {

	// 动态调整协程对象的数量，如果过多了，就自动把自己销毁
	if capacity := p.Cap(); (capacity > 0 && p.Running() > capacity) || p.IsClosed() {
		p.cond.Broadcast()
		return false
	}

	// 修改worker的最后一次使用时间（这里相当于回收放入到 workerqueue 中
	worker.lastUsed = p.nowTime()

	p.lock.Lock()
	defer p.lock.Unlock()
	// To avoid memory leaks, add a double check in the lock scope.
	// Issue: https://github.com/panjf2000/ants/issues/113
	if p.IsClosed() {
		return false
	}
	if err := p.workers.insert(worker); err != nil {
		return false
	}
	// Notify the invoker stuck in 'retrieveWorker()' of there is an available worker in the worker queue.
	p.cond.Signal()

	return true
}
