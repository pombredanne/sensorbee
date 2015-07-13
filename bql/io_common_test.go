package bql

import (
	"fmt"
	"pfi/sensorbee/sensorbee/core"
	"pfi/sensorbee/sensorbee/data"
	"sync"
	"time"
)

// mkTuples creates a slice of `num` Tuples with increasing
// timestamp and Data that holds a strictly increasing int
// value at the "int" key.
func mkTuples(num int) []*core.Tuple {
	tuples := make([]*core.Tuple, 0, num)
	for i := 0; i < num; i++ {
		tup := core.Tuple{
			Data: data.Map{
				"int": data.Int(i + 1),
			},
			InputName:     "input",
			Timestamp:     time.Date(2015, time.April, 10, 10, 23, i, 0, time.UTC),
			ProcTimestamp: time.Date(2015, time.April, 10, 10, 24, i, 0, time.UTC),
			BatchID:       7,
		}
		tuples = append(tuples, &tup)
	}
	return tuples
}

// createDummySource creates a source that emits a number
// of Tuples as generated by mkTuples. The number of Tuples
// is 4 by default, but can be changed using the "num" key of
// the params map.
func createDummySource(ctx *core.Context, params data.Map) (core.Source, error) {
	numTuples := 4
	// check the given source parameters
	for key, value := range params {
		if key == "num" {
			numTuples64, err := data.AsInt(value)
			if err != nil {
				msg := "num: cannot convert value %s into integer"
				return nil, fmt.Errorf(msg, value)
			}
			numTuples = int(numTuples64)
		} else {
			return nil, fmt.Errorf("unknown source parameter: %s", key)
		}
	}

	s := &tupleEmitterSource{Tuples: mkTuples(numTuples)}
	s.c = sync.NewCond(&s.m)
	return core.NewRewindableSource(s), nil
}

// tupleEmitterSource is a source that emits all tuples in the given
// slice when GenerateStream is called.
type tupleEmitterSource struct {
	Tuples []*core.Tuple
	m      sync.Mutex
	c      *sync.Cond

	// 0: running, 1: stopping, 2: stopped
	state int
}

func (s *tupleEmitterSource) GenerateStream(ctx *core.Context, w core.Writer) error {
	s.m.Lock()
	s.state = 0
	s.m.Unlock()

	defer func() {
		s.m.Lock()
		defer s.m.Unlock()
		s.state = 2
		s.c.Broadcast()
	}()

	for _, t := range s.Tuples {
		s.m.Lock()
		if s.state > 0 {
			s.state = 2
			s.c.Broadcast()
			s.m.Unlock()
			break
		}
		s.m.Unlock()

		if err := w.Write(ctx, t.Copy()); err != nil {
			if err == core.ErrSourceRewound || err == core.ErrSourceStopped {
				return err
			}
		}
	}
	return nil
}

func (s *tupleEmitterSource) Stop(ctx *core.Context) error {
	s.m.Lock()
	defer s.m.Unlock()
	if s.state == 2 {
		return nil
	}
	s.state = 1
	s.c.Broadcast()
	for s.state < 2 {
		s.c.Wait()
	}
	return nil
}

// createDummyUpdatableSource creates a source that
// can be updated source. It creates tupleEmitterSource inherited object.
func createDummyUpdatableSource(ctx *core.Context, params data.Map) (core.Source, error) {
	numTuples := 4
	// check the given source parameters
	for key, value := range params {
		if key == "num" {
			numTuples64, err := data.AsInt(value)
			if err != nil {
				msg := "num: cannot convert value %s into integer"
				return nil, fmt.Errorf(msg, value)
			}
			numTuples = int(numTuples64)
		} else {
			return nil, fmt.Errorf("unknown source parameter: %s", key)
		}
	}

	s := &tupleEmitterUpdatableSource{tupleEmitterSource: &tupleEmitterSource{Tuples: mkTuples(numTuples)}}
	s.c = sync.NewCond(&s.m)
	return s, nil
}

type tupleEmitterUpdatableSource struct {
	*tupleEmitterSource
}

func (s *tupleEmitterUpdatableSource) Update(params data.Map) error {
	return nil
}

func init() {
	RegisterGlobalSourceCreator("dummy", SourceCreatorFunc(createDummySource))
	RegisterGlobalSourceCreator("dummy_updatable", SourceCreatorFunc(createDummyUpdatableSource))
}

// createCollectorSink creates a sink that collects all received
// tuples in an internal array.
func createCollectorSink(ctx *core.Context, params data.Map) (core.Sink, error) {
	// check the given sink parameters
	for key := range params {
		return nil, fmt.Errorf("unknown sink parameter: %s", key)
	}
	si := tupleCollectorSink{}
	si.c = sync.NewCond(&si.m)
	return &si, nil
}

type tupleCollectorSink struct {
	Tuples []*core.Tuple
	m      sync.Mutex
	c      *sync.Cond
}

func (s *tupleCollectorSink) Write(ctx *core.Context, t *core.Tuple) error {
	if s.c == nil { // This is for old tests
		s.Tuples = append(s.Tuples, t)
		return nil
	}
	s.m.Lock()
	defer s.m.Unlock()
	s.Tuples = append(s.Tuples, t)
	s.c.Broadcast()
	return nil
}

// Wait waits until the collector receives at least n tuples.
func (s *tupleCollectorSink) Wait(n int) {
	s.m.Lock()
	defer s.m.Unlock()
	for len(s.Tuples) < n {
		s.c.Wait()
	}
}

func (s *tupleCollectorSink) Close(ctx *core.Context) error {
	return nil
}

func init() {
	RegisterGlobalSinkCreator("collector", SinkCreatorFunc(createCollectorSink))
}
