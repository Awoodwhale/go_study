---
markdown:
  image_dir: /go_context/imgs
  path: context.md
  ignore_from_front_matter: true
  absolute_image_path: false
---
# Golang context 实现原理

[TOC]

## 前言

context是golang中的经典工具，主要在异步场景中来实现并发协议，并且对goroutine的生命周期进行控制，同时其还有一定的存储能力。

## 1、Core data struct

### 1.1 context.Context

Context接口的源码定义：

```go
type Context interface {
    Deadline() (deadline time.Time, ok bool)
    Done() <-chan struct{}
    Err() error
    Value(key any) any
}
```

实现上述四个方法的就是一个Context类型：

1. Deadline，用来管理生命周期，返回context的过期时间
2. Done，返回context中的channel。使用`struct{}`来当作chan的类型是因为`struct{}`在golang中是共享的，不会分配更多的内存
3. Err，返回一整个context中出现的错误。常见的错误是`过期`和`被取消`
4. Value，返回context中存储的对应key的值

### 1.2 标准error

主要是两种错误：被取消或过期
```go
// Canceled is the error returned by Context.Err when the context is canceled.
var Canceled = errors.New("context canceled")

// DeadlineExceeded is the error returned by Context.Err when the context's
// deadline passes.
var DeadlineExceeded error = deadlineExceededError{}

type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "context deadline exceeded" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return true }
```

- Canceled：context被cancel的时候会报错
- DeadlineExceeded：context超时会报错

## 2、emptyCtx

### 2.1 emptyCtx data struct

最基本的一个context就是emptyCtx，实现的方式如下：

```go
type emptyCtx int

func (*emptyCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (*emptyCtx) Done() <-chan struct{} {
	return nil
}

func (*emptyCtx) Err() error {
	return nil
}

func (*emptyCtx) Value(key any) any {
	return nil
}
```

- emptyCtx是一个空的context，本质上类型是一个int
- Deadline返回的是一个公元元年的时间以及一个false的flag，表示当前context不存在过期时间
- Done返回一个nil，用户无论往nil中写还是读，都会陷入阻塞
- Err返回一个nil错误
- Value返回一个nil的值

### 2.2 context.Background() && context.TODO()

源码中的定义如下：
```go
var (
	background = new(emptyCtx)
	todo       = new(emptyCtx)
)

// Background returns a non-nil, empty Context. It is never canceled, has no
// values, and has no deadline. It is typically used by the main function,
// initialization, and tests, and as the top-level Context for incoming
// requests.
func Background() Context {
	return background
}

// TODO returns a non-nil, empty Context. Code should use context.TODO when
// it's unclear which Context to use or it is not yet available (because the
// surrounding function has not yet been extended to accept a Context
// parameter).
func TODO() Context {
	return todo
}
```

使用new的方式生成一个emptyCtx实例的指针

## 3、cancelCtx

### 3.1 cancelCtx data struct

```go
// A cancelCtx can be canceled. When canceled, it also cancels any children
// that implement canceler.
type cancelCtx struct {
	Context

	mu       sync.Mutex            // protects following fields
	done     atomic.Value          // of chan struct{}, created lazily, closed by first cancel call
	children map[canceler]struct{} // set to nil by the first cancel call
	err      error                 // set to non-nil by the first cancel call
}

// A canceler is a context type that can be canceled directly. The
// implementations are *cancelCtx and *timerCtx.
type canceler interface {
	cancel(removeFromParent bool, err error)
	Done() <-chan struct{}
}
```

- 在结构体中注入Context，实现了继承
- mu，互斥锁，可能过程存在需要上锁的情况
- done，是一个atomic.Value类型，其实就是 `<- chan struct{}`
- children，是一个存储子context的集合
- err，记录了当前cancelCtx的错误，一定是某个context的子context
- canceler记录实现cancel和Done函数的接口，只注重实现这两个函数

### 3.2 Deadline

cancelCtx没有实现Deadline的方法，仅仅是内嵌了一个带有Deadline方法的context接口，如果直接调用会报错

### 3.3 Done

使用`懒加载`的方式获取`done`字段
- 如果存在，那么直接返回
- 如果不存在就在加锁的条件下make一个并且存储进来

```go
func (c *cancelCtx) Done() <-chan struct{} {
	d := c.done.Load()
	if d != nil {
		return d.(chan struct{})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d = c.done.Load()
	if d == nil {
		d = make(chan struct{})
		c.done.Store(d)
	}
	return d.(chan struct{})
}
```

### 3.4 Err

加锁的情况下获取cancelCtx中的err

```go
func (c *cancelCtx) Err() error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	return err
}
```

### 3.5 Value

cancelCtx的Value方法有一个小trick，就是这个`cancelCtxKey`

`cancelCtxKey`是一个全局int变量，作用是用来判断是否需要返回自身这个对象

由于golang的export机制，小写开头的变量在其他的包是无法获取的，所以很明显`&cancelCtxKey`的操作是给context包内部的cancelCtx使用的

而使用这个trick的目的会在之后 `3.6.3` 讲到

```go
// &cancelCtxKey is the key that a cancelCtx returns itself for.
var cancelCtxKey int

func (c *cancelCtx) Value(key any) any {
	if key == &cancelCtxKey {
		return c
	}
	return value(c.Context, key)
}
```

### 3.6 context.WithCancel()

#### 3.6.1 context.WithCancel()

一定要传入一个parent的Context，否则会panic

之后的过程就是新建一个cancelCtx，然后调用`propagateCancel`，最后返回新建的ctx和一个cancel闭包函数

- newCancelCtx函数的作用是，以parent为父ctx，生成一个新的子ctx实例
- propagateCancel函数的作用是，让parent与children绑定，保证parent终止的时候，children也会终止
- cancel闭包函数的作用是，可以让用户调用此函数`让该cancelCtx终止`

```go
// WithCancel returns a copy of parent with a new Done channel. The returned
// context's Done channel is closed when the returned cancel function is called
// or when the parent context's Done channel is closed, whichever happens first.
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	c := newCancelCtx(parent)
	propagateCancel(parent, &c)
	return &c, func() { c.cancel(true, Canceled) }
}
```

#### 3.6.2 newCancelCtx()

非常的易懂，使用parent作为父ctx返回一个cancelCtx实例

```go
// newCancelCtx returns an initialized cancelCtx.
func newCancelCtx(parent Context) cancelCtx {
	return cancelCtx{Context: parent}
}
```

#### 3.6.3 propagateCancel()

源码较长，一步一步分析！

```go
// propagateCancel arranges for child to be canceled when parent is.
func propagateCancel(parent Context, child canceler) {
	done := parent.Done()
	if done == nil {
		return // parent is never canceled
	}

	select {
	case <-done:
		// parent is already canceled
		child.cancel(false, parent.Err())
		return
	default:
	}

	if p, ok := parentCancelCtx(parent); ok {
		p.mu.Lock()
		if p.err != nil {
			// parent has already been canceled
			child.cancel(false, p.err)
		} else {
			if p.children == nil {
				p.children = make(map[canceler]struct{})
			}
			p.children[child] = struct{}{}
		}
		p.mu.Unlock()
	} else {
		atomic.AddInt32(&goroutines, +1)
		go func() {
			select {
			case <-parent.Done():
				child.cancel(false, parent.Err())
			case <-child.Done():
			}
		}()
	}
}
```

1. 如果parent是不会被cancel的类型（例如emptyCtx），那么直接return
2. 进入select，如果parent已经被cancel了，那么就让child也cancel，同时携带parent的err
3. 进入`parentCancelCtx(parent)`函数，判断当前这个`parent是否是cancelCtx类型`
4. 如果parent是cancelCtx类型：
	- 首先加锁，再执行如下操作
	- parent已经存在err了，说明父ctx已经被cancel了，让child也cancel，携带上parent的err
	- 父ctx没有cancel，那么让当前child加入parent的children集合
	- 完成上述操作，解锁
5. 如果parent不是cancelCtx类型：
	- 不是cancelCtx，但是存在cancel的能力（用户自己实现的ctx）
	- 启动一个协程，多路复用的方式监听parent的状态，如果parent被cancel了，那么child也cancel
	- 同时也要监听child.Done()，因为如果一个父ctx的多个子ctx同时cancel了，如果不处理child.Done()，那么可能会造成goroutine泄露

上述操作提到了一个`parentCancelCtx(parent)`函数，这个函数用来判断parent是否是`cancelCtx`类型，如何做到的？源码如下：

```go
// parentCancelCtx returns the underlying *cancelCtx for parent.
// It does this by looking up parent.Value(&cancelCtxKey) to find
// the innermost enclosing *cancelCtx and then checking whether
// parent.Done() matches that *cancelCtx. (If not, the *cancelCtx
// has been wrapped in a custom implementation providing a
// different done channel, in which case we should not bypass it.)
func parentCancelCtx(parent Context) (*cancelCtx, bool) {
	done := parent.Done()
	if done == closedchan || done == nil {
		return nil, false
	}
	p, ok := parent.Value(&cancelCtxKey).(*cancelCtx)
	if !ok {
		return nil, false
	}
	pdone, _ := p.done.Load().(chan struct{})
	if pdone != done {
		return nil, false
	}
	return p, true
}
```

1. 如果parent已经cancel或者不具备cancel的能力，那么就返回false
2. 通过`cancelCtxKey`这个特定的协议获取cancelCtx自身，如果获取不到自身，就不是cancelCtx
3. 如果能获取到自身，还得先判断两个done是否相等，不相等返回false，相等返回ctx和true

#### 3.6.4 cancelCtx.cancel()

`cancel`函数有两个参数：

- removeFromParent bool：是否要将此ctx从其父ctx的child集合中删除
- err error：传入之所以要取消的理由（携带err来告知）

```go
// closedchan is a reusable closed channel.
var closedchan = make(chan struct{})

// cancel closes c.done, cancels each of c's children, and, if
// removeFromParent is true, removes c from its parent's children.
func (c *cancelCtx) cancel(removeFromParent bool, err error) {
	if err == nil {
		panic("context: internal error: missing cancel error")
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return // already canceled
	}
	c.err = err
	d, _ := c.done.Load().(chan struct{})
	if d == nil {
		c.done.Store(closedchan)
	} else {
		close(d)
	}
	for child := range c.children {
		// NOTE: acquiring the child's lock while holding parent's lock.
		child.cancel(false, err)
	}
	c.children = nil
	c.mu.Unlock()

	if removeFromParent {
		removeChild(c.Context, c)
	}
}


// removeChild removes a context from its parent.
func removeChild(parent Context, child canceler) {
	p, ok := parentCancelCtx(parent)
	if !ok {
		return
	}
	p.mu.Lock()
	if p.children != nil {
		delete(p.children, child)
	}
	p.mu.Unlock()
}

```

简略看完源码后来看看源码的逻辑：

1. err不能未空
2. 上锁
3. 如果当前ctx已经存在err了，也就是已经cancel了，直接return
4. 当前ctx不存在err，将err赋值给ctx.err
5. close done，传递信号。分两个情况，如果done是nil，那么存一个closedchan，否则直接close
6. 遍历child集合，将子ctx全部cancel
7. 释放ctx的child集合，置为nil
8. 解锁
9. 如果需要从其父ctx的child集合中删除，就去删除。删除的细节如下：
	- 判断ctx是否是cancelCtx，不是就直接返回
	- 上锁
	- 如果有children集合，那么久把子ctx从其中删除
	- 解锁

## 4、timerCtx

### 4.1 timerCtx data struct

timerCtx是`继承自cancelCtx`，在此基础上，多了一个Deadline的实现

- timer *time.Timer: 用于在过期时间来终止ctx
- deadline time.Time: 设定的终止时间

```go
type timerCtx struct {
	cancelCtx
	timer *time.Timer // Under cancelCtx.mu.

	deadline time.Time
}
```

### 4.2 timerCtx.Deadline()

context.Context interface下的`Deadline()`仅在timerCtx中实现了，用于显示其过期时期

```go
func (c *timerCtx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, true
}
```

### 4.3 timerCtx.cancel()

timerCtx的cancel函数两个参数的含义和cancelCtx的cancel一样。流程如下：

1. 调用cancelCtx的cancel
2. 如果需要从父ctx中删除，就调用removeChild
3. 加锁
4. 如果timerCtx的timer不为空，那么就去关闭这个timer，并且置空这个timer
5. 解锁

```go
func (c *timerCtx) cancel(removeFromParent bool, err error) {
	c.cancelCtx.cancel(false, err)
	if removeFromParent {
		// Remove this timerCtx from its parent cancelCtx's children.
		removeChild(c.cancelCtx.Context, c)
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}
```

### 4.4 context.WithTimeout && context.WithDeadline

`WithTimeout`方法用于构造一个timerCtx，本质上是去调用`WithDeadline`方法

```go
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return WithDeadline(parent, time.Now().Add(timeout))
}
```

通过源码来分析WithDeadline的流程

```go
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if cur, ok := parent.Deadline(); ok && cur.Before(d) {
		// The current deadline is already sooner than the new one.
		return WithCancel(parent)
	}
	c := &timerCtx{
		cancelCtx: newCancelCtx(parent),
		deadline:  d,
	}
	propagateCancel(parent, c)
	dur := time.Until(d)
	if dur <= 0 {
		c.cancel(true, DeadlineExceeded) // deadline has already passed
		return c, func() { c.cancel(false, Canceled) }
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.timer = time.AfterFunc(dur, func() {
			c.cancel(true, DeadlineExceeded)
		})
	}
	return c, func() { c.cancel(true, Canceled) }
}
```

1. 校验parent非空
2. 调用parent.Deadline()获取过期时间，如果想要设定的时间比过期时间还晚，直接返回一个cancelCtx，而不设置过期时间
3. 如果过期时间合理，那么创建一个timerCtx
4. 将新建的timerCtx与父ctx进行绑定
5. 判断过期时间是否到了，如果到了进行cancel timerCtx的操作，并返回`DeadlineExceeded`的错误
6. 加锁
8. 如果没有cancel，就启动`time.AfterFunc`，设定一个延时执行函数，其中执行的是cancel方法，也就是过期自动cancel
9. 解锁
10. 返回timerCtx和一个闭包cancel函数

## 5、valueCtx

### 5.1 valueCtx data struct

非常的简单，继承一个Context，同时有且仅有一个key、value对

```go
type valueCtx struct {
	Context
	key, val any
}
```

### 5.2 valueCtx.Value()

既然叫作valueCtx，没点存储value的能力怎么能行？

```go
func (c *valueCtx) Value(key any) any {
	if c.key == key {
		return c.val
	}
	return value(c.Context, key)
}

func value(c Context, key any) any {
	for {
		switch ctx := c.(type) {
		case *valueCtx:
			if key == ctx.key {
				return ctx.val
			}
			c = ctx.Context
		case *cancelCtx:
			if key == &cancelCtxKey {
				return c
			}
			c = ctx.Context
		case *timerCtx:
			if key == &cancelCtxKey {
				return &ctx.cancelCtx
			}
			c = ctx.Context
		case *emptyCtx:
			return nil
		default:
			return c.Value(key)
		}
	}
}
```
分析一下上述存储的流程：

1. 如果当前ctx的key == 传入的key，直接返回当前节点的value
2. 如果不是的话，步入value()函数
3. 无限for循环，直到找到值，或者到了最顶层的emptyCtx，返回nil
4. switch判断类型，如果是valueCtx，一步一步往上层ctx走
5. 如果是碰到了cancelCtxKey，那么走到了cancelCtx或timerCtx的位置就返回
6. 如果走到了顶层的emptyCtx，直接返回nil

总体来说，上述找value的过程是一个迭代的思想，从子context不断往上走，找父context中是否存在key对应的value

### 5.3 valueCtx用法小结

从上述源码阅读来看，valueCtx虽然具有存储value的能力，但仅仅局限于少量数据，例如http头部请求这种。原因有如下三点：

1. 一个valueCtx中只有一个键值对，存放的内容很少，同时每次生成valueCtx的时候都是新建一个节点，非常消耗空间资源
2. 通过key寻找value的时间复杂度是O(n)，相当于循环遍历，从子ctx一直遍历到最顶层的父ctx
3. 并不支持key的去重机制，所以如果key相同的话，是根据寻找节点的位置返回value，可能造成value不同的情况。

### 5.4 context.WithValue()

源码如下：

```go
func WithValue(parent Context, key, val any) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if key == nil {
		panic("nil key")
	}
	if !reflectlite.TypeOf(key).Comparable() {
		panic("key is not comparable")
	}
	return &valueCtx{parent, key, val}
}
```

获取流程：

1. 判断parent非空
2. 判断key非空
3. 判断key是否Comparable
4. 满足上述所有条件后，创建一个子ctx节点并返回，其中存放一个键值对