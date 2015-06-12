package core

import (
	"fmt"
	. "github.com/smartystreets/goconvey/convey"
	"pfi/sensorbee/sensorbee/core/tuple"
	"sync"
	"testing"
)

type panicBox struct {
	ProxyBox

	m            sync.Mutex
	writeFailAt  int
	writePanicAt int
	writeCnt     int
}

func (b *panicBox) Process(ctx *Context, t *tuple.Tuple, w Writer) error {
	b.m.Lock()
	defer b.m.Unlock()
	b.writeCnt++
	if b.writeCnt == b.writePanicAt {
		panic(fmt.Errorf("test failure via panic"))
	}
	if b.writeCnt == b.writeFailAt {
		return fmt.Errorf("test failure")
	}
	return b.ProxyBox.Process(ctx, t, w)
}

func TestDefaultDynamicTopologyFailure(t *testing.T) {
	config := Configuration{TupleTraceEnabled: 1}
	ctx := newTestContext(config)

	Convey("Given a simple linear topology", t, func() {
		/*
		 *   so -*--> b1 -*--> si
		 */
		dt := NewDefaultDynamicTopology(ctx, "dt1")
		t := dt.(*defaultDynamicTopology)
		Reset(func() {
			t.Stop()
		})

		so := NewTupleIncrementalEmitterSource(freshTuples())
		_, err := t.AddSource("source", so, nil)
		So(err, ShouldBeNil)

		b1 := &panicBox{
			ProxyBox: ProxyBox{
				b: &BlockingForwardBox{cnt: 8},
			},
		}
		tc1 := newTerminateChecker(b1)
		bn1, err := t.AddBox("box1", tc1, nil)
		So(err, ShouldBeNil)
		So(bn1.Input("source", nil), ShouldBeNil)

		si := NewTupleCollectorSink()
		sic := &sinkCloseChecker{s: si}
		sin, err := t.AddSink("sink", sic, nil)
		So(err, ShouldBeNil)
		So(sin.Input("box1", nil), ShouldBeNil)

		Convey("When a box panics", func() {
			b1.writePanicAt = 1
			so.EmitTuples(5)

			Convey("Then the box stops", func() {
				So(bn1.State().Wait(TSStopped), ShouldEqual, TSStopped)
			})

			Convey("Then the topology can be recovered by manual connection", func() {
				So(sin.Input("source", nil), ShouldBeNil)
				so.EmitTuples(3)
				si.Wait(3)
				So(len(si.Tuples), ShouldEqual, 3)
			})
		})

		// TODO: add more fail tests!!
	})
}
