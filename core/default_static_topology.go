package core

import (
	"fmt"
	"pfi/sensorbee/sensorbee/core/tuple"
	"strings"
	"sync"
)

type defaultStaticTopology struct {
	srcs  map[string]Source
	boxes map[string]Box
	sinks map[string]Sink

	// srcDsts have a set of destinations to which each source write tuples.
	// This is necessary because Source cannot directly close writers and
	// defaultStaticTopology has to take care of it.
	//
	// srcDsts will not be thread-safe once the topology started running.
	srcDsts      map[string]WriteCloser
	srcDstsMutex sync.Mutex

	// nodes holds a staticNode for each box or sink in the topology.
	nodes map[string]*staticNode

	state      TopologyState
	stateMutex *sync.Mutex
	stateCond  *sync.Cond

	fatalListenerMutex sync.Mutex
	fatalListener      []func(ctx *Context, name string, err error)
}

func (t *defaultStaticTopology) Run(ctx *Context) error {
	checkState := func() error { // A closure is used to perform defer
		t.stateMutex.Lock()
		defer t.stateMutex.Unlock()
		switch t.state {
		case TSInitialized:
		case TSStarting:
			// Immediately returning an error could be confusing for callers,
			// so wait until the topology becomes at least the running state.
			if _, err := t.wait(TSRunning); err != nil {
				return err
			}

			// It's natural for Run to return an error when the state isn't
			// TSInitialized even if it's TSStarting so that only a single
			// caller will succeed.
			fallthrough

		default:
			return fmt.Errorf("the static topology has already started")
		}
		t.state = TSStarting
		t.stateCond.Broadcast()
		return nil
	}
	if err := checkState(); err != nil {
		return err
	}

	// Don't move this defer to the head of the method otherwise calling
	// Run method when the state is TSRunning will set TSStopped without
	// actually stopping the topology.
	defer t.setState(TSStopped)

	// Initialize boxes in advance.
	var inited []string
	for name, box := range t.boxes {
		sbox, ok := box.(StatefulBox)
		if !ok {
			continue
		}

		if err := sbox.Init(ctx); err != nil {
			// Terminate all Boxes initialized so far.
			for _, n := range inited {
				func() {
					defer func() {
						if e := recover(); e != nil {
							ctx.Logger.Log(Error, "Termination of box %v failed by panic: %v", n, e)
						}
					}()
					if err := t.boxes[n].(StatefulBox).Terminate(ctx); err != nil {
						ctx.Logger.Log(Error, "Termination of box %v failed: %v", n, err)
					}
				}()
			}
			return err
		}
		inited = append(inited, name)
	}
	return t.run(ctx)
}

func (t *defaultStaticTopology) setState(s TopologyState) {
	t.stateMutex.Lock()
	defer t.stateMutex.Unlock()
	t.state = s
	t.stateCond.Broadcast()
}

// run spawns goroutines for sources, boxes, and sinks.
// The caller of it must set TSStop after it returns.
func (t *defaultStaticTopology) run(ctx *Context) error {
	// Run goroutines for each node(boxes and sinks).
	var wg sync.WaitGroup
	for name, node := range t.nodes {
		wg.Add(1)
		go func(name string, node *staticNode) {
			defer wg.Done()
			node.run(ctx, t, name)
		}(name, node)
	}

	// Create closures for goroutines here because srcDsts will not
	// be thread-safe once those goroutines start.
	fs := make([]func(), 0, len(t.srcs))
	for name, src := range t.srcs {
		name := name
		src := src
		dst := t.srcDsts[name]
		f := func() {
			defer wg.Done()
			defer func() {
				if err := recover(); err != nil {
					ctx.Logger.Log(Error, "%v paniced: %v", name, err)
				}

				// Because dst could be closed by defaultStaticTopology.Stop when
				// Source.Stop failed in that method, closing dst must be done
				// via closeDestination method.
				if err := t.closeDestination(ctx, name); err != nil {
					ctx.Logger.Log(Error, "%v cannot close the destination: %v", name, err)
				}
			}()
			if err := src.GenerateStream(ctx, newTraceWriter(dst, tuple.Output, name)); err != nil {
				ctx.Logger.Log(Error, "%v cannot generate tuples: %v", name, err)
			}
		}
		fs = append(fs, f)
	}
	for _, f := range fs {
		wg.Add(1)
		go f()
	}

	t.setState(TSRunning)
	wg.Wait()
	return nil
}

func (t *defaultStaticTopology) closeDestination(ctx *Context, src string) error {
	dst := func() WriteCloser {
		t.srcDstsMutex.Lock()
		defer t.srcDstsMutex.Unlock()

		// Since WriteCloser.Close doesn't have to be idempotent, it should
		// only be called exactly once.
		dst, ok := t.srcDsts[src]
		if !ok {
			return nil
		}
		delete(t.srcDsts, src)
		return dst
	}()

	// srcDstsMutex is unlocked here.
	if dst == nil {
		return nil
	}
	return dst.Close(ctx)
}

// State returns the current state of the topology. See TopologyState for details.
func (t *defaultStaticTopology) State(ctx *Context) TopologyState {
	t.stateMutex.Lock()
	defer t.stateMutex.Unlock()
	return t.state
}

// Wait waits until the topology has the specified state. It returns the
// current state of the topology. The current state may differ from the
// given state, but it's guaranteed that the current state is a successor of
// the given state when error is nil. For example, when Wait(TSStarting) is
// is called, TSRunning or TSStopped can be returned.
func (t *defaultStaticTopology) Wait(ctx *Context, s TopologyState) (TopologyState, error) {
	t.stateMutex.Lock()
	defer t.stateMutex.Unlock()
	return t.wait(s)
}

// wait is the internal version of Wait. It assumes the caller
// has already acquired the lock of stateMutex.
func (t *defaultStaticTopology) wait(s TopologyState) (TopologyState, error) {
	for t.state < s {
		t.stateCond.Wait()
	}
	return t.state, nil
}

func (t *defaultStaticTopology) Stop(ctx *Context) error {
	stopped, err := func() (bool, error) {
		t.stateMutex.Lock()
		defer t.stateMutex.Unlock()
		for {
			switch t.state {
			case TSInitialized:
				t.state = TSStopped
				t.stateCond.Broadcast()
				return true, nil

			case TSStarting:
				if _, err := t.wait(TSRunning); err != nil {
					return false, err
				}
				// If somebody else has already stopped the topology,
				// the state might be different from TSRunning. So, this process
				// continues to the next iteration.

			case TSRunning:
				t.state = TSStopping
				t.stateCond.Broadcast()
				return false, nil

			case TSStopping:
				// Someone else is trying to stop the topology. This thread
				// just waits until it's stopped.
				_, err := t.wait(TSStopped)
				return true, err

			case TSStopped:
				return true, nil

			default:
				return false, fmt.Errorf("the static topology has an invalid state: %v", t.state)
			}
		}
	}()
	if err != nil {
		return err
	} else if stopped {
		return nil
	}

	var stopFailures []string
	for name, src := range t.srcs {
		errHandler := func() {
			stopFailures = append(stopFailures, name)
			if err := t.closeDestination(ctx, name); err != nil {
				ctx.Logger.Log(Error, "Cannot close the source %v's destination: %v", name, err)
			}
		}
		func() {
			defer func() {
				if e := recover(); e != nil {
					ctx.Logger.Log(Error, "Cannot stop source %v due to panic: %v", name, err)
					errHandler()
				}
			}()
			if err := src.Stop(ctx); err != nil {
				ctx.Logger.Log(Error, "Cannot stop source %v: %v", name, err)
				errHandler()
			}
		}()
	}

	// TODO: There might be some WriteClosers which still haven't been closed
	// and some nodes attached to them are still running. There might
	// have to be some way to force shutdown nodes.

	// Once all sources are stopped, the stream will eventually stop.
	t.stateMutex.Lock()
	defer t.stateMutex.Unlock()
	if err == nil && len(stopFailures) > 0 {
		// If some sources couldn't be stopped, t.wait(TSStopped) would block forever.
		// So, the state must be set TSStopped here although Run is still running,
		t.state = TSStopped
		t.stateCond.Broadcast()
		return fmt.Errorf("%v sources couldn't be stopped but the topology has stopped: failed sources = %v",
			len(stopFailures), strings.Join(stopFailures, ", "))
	}
	_, err = t.wait(TSStopped)
	return err
}

// AddFatalListener adds a listener function to the topology. The listener is
// called when a source, a box, or a sink returned a fatal error. Listeners
// are never called concurrently, that is they don't have to acquire locks even
// if two fatal errors occurred at the same time.
//
// Currently, a fatal error occurred in Source.GenerateStream are not reported.
func (t *defaultStaticTopology) AddFatalListener(l func(ctx *Context, name string, err error)) {
	t.fatalListenerMutex.Lock()
	defer t.fatalListenerMutex.Unlock()
	t.fatalListener = append(t.fatalListener, l)
}

func (t *defaultStaticTopology) notifyFatalListeners(ctx *Context, name string, err error) {
	t.fatalListenerMutex.Lock()
	defer t.fatalListenerMutex.Unlock()

	for _, l := range t.fatalListener {
		l(ctx, name, err)
	}
}

type staticPipeReceiver struct {
	in <-chan *tuple.Tuple
}

type staticPipeSender struct {
	inputName string
	out       chan<- *tuple.Tuple // TODO: this should be []*tuple.Tuple for efficiency
}

func (s *staticPipeSender) Write(ctx *Context, t *tuple.Tuple) error {
	t.InputName = s.inputName
	s.out <- t
	return nil
}

func (s *staticPipeSender) Close(ctx *Context) error {
	close(s.out)
	return nil
}

func newStaticPipe(inputName string, capacity int) (*staticPipeReceiver, *staticPipeSender) {
	p := make(chan *tuple.Tuple, capacity)

	r := &staticPipeReceiver{
		in: p,
	}
	s := &staticPipeSender{
		inputName: inputName,
		out:       p,
	}
	return r, s
}

// staticNode has a box or sink and it receives tuples from the previous node in
// the topology and writes tuples to the next destination nodes. It allows boxes
// and sinks to receive tuples from multiple sources and boxes.
type staticNode struct {
	// dst is a writer which sends tuples to the real destination.
	// dst can be a box, a sink.
	dst WriteCloser

	// inputs is the input channels
	inputs map[string]*staticPipeReceiver
}

func newStaticNode(dst WriteCloser) *staticNode {
	return &staticNode{
		dst:    dst,
		inputs: map[string]*staticPipeReceiver{},
	}
}

func (sc *staticNode) addInput(name string, in *staticPipeReceiver) {
	sc.inputs[name] = in
}

func (sc *staticNode) run(ctx *Context, topology *defaultStaticTopology, name string) {
	var (
		wg                sync.WaitGroup
		fatalErrorHandler sync.Once
	)

	setFatal := func(v interface{}) {
		err, ok := v.(error)
		if ok {
			if !IsFatalError(err) {
				err = FatalError(err)
			}
		} else {
			err = FatalError(fmt.Errorf("unknown error through panic: %v", v))
		}
		topology.notifyFatalListeners(ctx, name, err)
	}

	drainer := func(in *staticPipeReceiver) {
		// This loop just consumes tuples to prevent the caller from being
		// stuck in the chan. This loop stops when the sender closes the
		// channel. Wish if the channel could be closed from the reader side
		// without causing panic on the writer side.
		//
		// According to chan's semantics (single-writer/multiple-readers),
		// staticPipeSender and Receiver should have another channel which
		// the Receiver can notify the Sender that it cannot receive tuples
		// anymore. However, defaultStaticTopology doesn't have to be that
		// complex.
		for _ = range in.in {
		}
	}

	for _, in := range sc.inputs {
		in := in

		wg.Add(1)
		// TODO: These goroutines should be assembled to one goroutine with reflect.Select.
		go func() {
			defer wg.Done()
			defer func() { // panic handler
				if err := recover(); err != nil {
					// Once a Box panics, it'll always panic after that. So,
					// the log should only be written once.
					fatalErrorHandler.Do(func() {
						ctx.Logger.Log(Error, "%v paniced: %v", name, err)
						go setFatal(err)
						go drainer(in)
					})
				}
			}()

		consumerLoop:
			for t := range in.in {
				// When a fatal error occurred in another goroutine, loops in
				// other goroutines don't stop immediately. They stop only when
				// the Box returns the fatal error again, or it panics.
				//
				// It's possible for these loops to have a shared atomic flag
				// to detect whether a fatal error is already returned or not.
				// However, it adds extra costs to tuple processing. Moreover,
				// stopping the topology here simply doesn't make sense because
				// a static topology can no longer work correctly if a fatal
				// error occurred. The topology will eventually be stopped by
				// its Stop method and it stops all data flows gracefully. Thus,
				// staticNode doesn't explicitly close both input and output and
				// waits until the topology tries to stop all.
				if err := sc.dst.Write(ctx, t); err != nil {
					switch {
					case IsFatalError(err):
						fatalErrorHandler.Do(func() {
							ctx.Logger.Log(Error, "%v had a fatal error: %v", name, err)
							go setFatal(err)
							go drainer(in)
						})
						break consumerLoop

					case IsTemporaryError(err):
						// TODO: retry

					default:
						ctx.Logger.Log(Warning, "%v cannot write a tuple: %v", name, err)
						// Skip the current tuple
					}
				}
			}
		}()
	}
	wg.Wait()

	if err := sc.dst.Close(ctx); err != nil {
		ctx.Logger.Log(Error, "%v cannot close its output channel: %v", name, err)
	}
}

type staticDestinations struct {
	names []string
	dsts  []WriteCloser
}

func newStaticDestinations() *staticDestinations {
	return &staticDestinations{}
}

// addDestination adds a WriteCloser as a destination.
// This method isn't thread-safe after the topology start flowing tuples.
func (mc *staticDestinations) addDestination(name string, w WriteCloser) {
	mc.names = append(mc.names, name)
	mc.dsts = append(mc.dsts, w)
}

func (mc *staticDestinations) Write(ctx *Context, t *tuple.Tuple) error {
	e := &bulkErrors{}
	needsCopy := len(mc.dsts) > 1
	for i, d := range mc.dsts {
		s := t
		if needsCopy {
			s = t.Copy()
		}
		if err := d.Write(ctx, s); err != nil {
			// Because all boxes and sinks are wrapped with staticNode,
			// staticDestinations doesn't have to handle retry here.

			// TODO: this error message could be a little redundant.
			e.append(fmt.Errorf("a tuple cannot be written to %v: %v", mc.names[i], err))
		}
	}
	return e.returnError()
}

func (mc *staticDestinations) Close(ctx *Context) error {
	e := &bulkErrors{}
	for i, d := range mc.dsts {
		if err := d.Close(ctx); err != nil {
			e.append(fmt.Errorf("output channel to %v cannot be closed: %v", mc.names[i], err))
		}
	}
	return e.returnError()
}

type boxWriterAdapter struct {
	box  Box
	name string
	dst  *traceWriter
}

func newBoxWriterAdapter(b Box, name string, dst WriteCloser) *boxWriterAdapter {
	return &boxWriterAdapter{
		box:  b,
		name: name,
		// An output traces is written just after the box Process writes a tuple.
		dst: newTraceWriter(dst, tuple.Output, name),
	}
}

func (wa *boxWriterAdapter) Write(ctx *Context, t *tuple.Tuple) error {
	tracing(t, ctx, tuple.Input, wa.name)
	return wa.box.Process(ctx, t, wa.dst)
}

func (wa *boxWriterAdapter) Close(ctx *Context) error {
	// TODO: handle panics
	var errb error
	if sbox, ok := wa.box.(StatefulBox); ok {
		errb = sbox.Terminate(ctx)
	}
	errw := wa.dst.w.Close(ctx)
	if errb != nil {
		return errb // An error from the Box is considered more important.
	}
	return errw
}

type traceWriter struct {
	w     WriteCloser
	inout tuple.EventType
	msg   string
}

func newTraceWriter(w WriteCloser, inout tuple.EventType, msg string) *traceWriter {
	return &traceWriter{
		w:     w,
		inout: inout,
		msg:   msg,
	}
}

func (tw *traceWriter) Write(ctx *Context, t *tuple.Tuple) error {
	tracing(t, ctx, tw.inout, tw.msg)
	return tw.w.Write(ctx, t)
}

func (tw *traceWriter) Close(ctx *Context) error {
	return tw.w.Close(ctx)
}