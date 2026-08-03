package rpcwire

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// OutboundPriority classifies frames for the optional dual-queue scheduler.
// Control frames may overtake queued data frames that have not started writing.
type OutboundPriority int

const (
	// PriorityControl is used for ping, cancel, mutations, subscribe recovery,
	// small control responses, and other latency-sensitive frames.
	PriorityControl OutboundPriority = iota
	// PriorityData is used for stream chunks, session events, and large query
	// responses. Data frames never overtake earlier data frames in the same queue.
	PriorityData
)

// OutboundFrameKind identifies the JSON-RPC frame shape for classification.
type OutboundFrameKind string

const (
	FrameRequest      OutboundFrameKind = "request"
	FrameNotification OutboundFrameKind = "notification"
	FrameResponse     OutboundFrameKind = "response"
	FrameError        OutboundFrameKind = "error"
)

// OutboundMeta describes a frame before it enters the optional outbound
// scheduler. Method may be empty for peer-driven error responses that are not
// tied to a known method.
type OutboundMeta struct {
	Kind   OutboundFrameKind
	Method string
}

// OutboundClassifier assigns a priority to one outbound frame.
type OutboundClassifier func(meta OutboundMeta) OutboundPriority

// OutboundSchedulerOptions configures the dual-queue outbound scheduler.
// When Options.Outbound is nil, Conn keeps the original synchronous write mutex.
type OutboundSchedulerOptions struct {
	Classifier OutboundClassifier
	// OnQueued is an optional deterministic-test/diagnostic hook. It runs after
	// a frame is committed to a queue and before the writer is signalled.
	OnQueued func(OutboundPriority)
	// ControlMaxFrames bounds the control queue length. Non-positive uses 128.
	ControlMaxFrames int
	// ControlMaxBytes bounds the total control queue payload. Non-positive uses 16 MiB.
	ControlMaxBytes int
	// DataMaxFrames bounds the data queue length. Non-positive uses 256.
	DataMaxFrames int
	// DataMaxBytes bounds the total data queue payload. Non-positive uses 32 MiB.
	DataMaxBytes int
	// ControlBurst is the number of consecutive control frames written before
	// one queued data frame must be admitted. Non-positive uses 16.
	ControlBurst int
}

// RemoteOutboundScheduler returns the Remote Workbench defaults from the
// recovery plan: control 128/16MiB, data 256/32MiB, control burst 16.
func RemoteOutboundScheduler(classifier OutboundClassifier) *OutboundSchedulerOptions {
	return &OutboundSchedulerOptions{
		Classifier:       classifier,
		ControlMaxFrames: 128,
		ControlMaxBytes:  16 << 20,
		DataMaxFrames:    256,
		DataMaxBytes:     32 << 20,
		ControlBurst:     16,
	}
}

func (o *OutboundSchedulerOptions) normalize() OutboundSchedulerOptions {
	if o == nil {
		return OutboundSchedulerOptions{}
	}
	out := *o
	if out.ControlMaxFrames <= 0 {
		out.ControlMaxFrames = 128
	}
	if out.ControlMaxBytes <= 0 {
		out.ControlMaxBytes = 16 << 20
	}
	if out.DataMaxFrames <= 0 {
		out.DataMaxFrames = 256
	}
	if out.DataMaxBytes <= 0 {
		out.DataMaxBytes = 32 << 20
	}
	if out.ControlBurst <= 0 {
		out.ControlBurst = 16
	}
	if out.Classifier == nil {
		out.Classifier = func(OutboundMeta) OutboundPriority { return PriorityControl }
	}
	return out
}

// OutboundQueueStats is a non-secret diagnostic snapshot of scheduler pressure.
type OutboundQueueStats struct {
	ControlFrames    int   `json:"controlFrames"`
	ControlBytes     int   `json:"controlBytes"`
	ControlHighWater int   `json:"controlHighWater"`
	DataFrames       int   `json:"dataFrames"`
	DataBytes        int   `json:"dataBytes"`
	DataHighWater    int   `json:"dataHighWater"`
	WaitedFrames     int64 `json:"waitedFrames"`
	WaitedNanos      int64 `json:"waitedNanos"`
	Overflows        int64 `json:"overflows"`
}

type outboundJob struct {
	data       []byte
	prio       OutboundPriority
	done       chan error
	enqueuedAt time.Time
}

type outboundScheduler struct {
	opts OutboundSchedulerOptions
	name string

	mu           sync.Mutex
	control      []outboundJob
	data         []outboundJob
	controlBytes int
	dataBytes    int
	controlHW    int
	dataHW       int
	controlBurst int
	closed       bool
	closeErr     error
	wake         chan struct{}

	waitedFrames atomic.Int64
	waitedNanos  atomic.Int64
	overflows    atomic.Int64

	writerOnce sync.Once
	writerDone chan struct{}
	writeFn    func([]byte) error
	failFn     func(error)
}

func newOutboundScheduler(name string, opts OutboundSchedulerOptions, writeFn func([]byte) error, failFn func(error)) *outboundScheduler {
	if name == "" {
		name = "rpcwire"
	}
	return &outboundScheduler{
		opts:       opts.normalize(),
		name:       name,
		wake:       make(chan struct{}, 1),
		writerDone: make(chan struct{}),
		writeFn:    writeFn,
		failFn:     failFn,
	}
}

func (s *outboundScheduler) start() {
	s.writerOnce.Do(func() {
		go s.loop()
	})
}

func (s *outboundScheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *outboundScheduler) enqueue(data []byte, prio OutboundPriority) error {
	s.start()
	job := outboundJob{
		data:       data,
		prio:       prio,
		done:       make(chan error, 1),
		enqueuedAt: time.Now(),
	}
	s.mu.Lock()
	if s.closed {
		err := s.closeErr
		s.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("%s: connection closed", s.name)
		}
		return err
	}
	size := len(data)
	switch prio {
	case PriorityData:
		if len(s.data)+1 > s.opts.DataMaxFrames || s.dataBytes+size > s.opts.DataMaxBytes {
			s.overflows.Add(1)
			err := fmt.Errorf("%s: outbound data queue overflow", s.name)
			s.closeLocked(err)
			s.mu.Unlock()
			if s.failFn != nil {
				s.failFn(err)
			}
			return err
		}
		s.data = append(s.data, job)
		s.dataBytes += size
		if n := len(s.data); n > s.dataHW {
			s.dataHW = n
		}
	default:
		if len(s.control)+1 > s.opts.ControlMaxFrames || s.controlBytes+size > s.opts.ControlMaxBytes {
			s.overflows.Add(1)
			err := fmt.Errorf("%s: outbound control queue overflow", s.name)
			s.closeLocked(err)
			s.mu.Unlock()
			if s.failFn != nil {
				s.failFn(err)
			}
			return err
		}
		s.control = append(s.control, job)
		s.controlBytes += size
		if n := len(s.control); n > s.controlHW {
			s.controlHW = n
		}
	}
	s.mu.Unlock()
	if s.opts.OnQueued != nil {
		s.opts.OnQueued(prio)
	}
	s.signal()
	return <-job.done
}

// fail closes the queues without waiting for the writer loop. Safe to call from
// the writer itself (overflow, write error) and from Conn.shutdown.
func (s *outboundScheduler) fail(err error) {
	s.mu.Lock()
	s.closeLocked(err)
	s.mu.Unlock()
	s.signal()
}

func (s *outboundScheduler) closeLocked(err error) {
	if s.closed {
		if s.closeErr == nil && err != nil {
			s.closeErr = err
		}
		return
	}
	s.closed = true
	s.closeErr = err
	term := err
	if term == nil {
		term = fmt.Errorf("%s: connection closed", s.name)
	}
	for i := range s.control {
		s.control[i].done <- term
	}
	for i := range s.data {
		s.data[i].done <- term
	}
	s.control = nil
	s.data = nil
	s.controlBytes = 0
	s.dataBytes = 0
}

func (s *outboundScheduler) loop() {
	defer close(s.writerDone)
	for {
		job, ok := s.dequeue()
		if !ok {
			return
		}
		if !job.enqueuedAt.IsZero() {
			waited := time.Since(job.enqueuedAt)
			s.waitedFrames.Add(1)
			s.waitedNanos.Add(waited.Nanoseconds())
		}
		writeErr := s.writeFn(job.data)
		job.done <- writeErr
		if writeErr != nil {
			// Close remaining waiters without blocking on this loop.
			s.fail(writeErr)
			if s.failFn != nil {
				s.failFn(writeErr)
			}
		}
	}
}

func (s *outboundScheduler) dequeue() (outboundJob, bool) {
	for {
		s.mu.Lock()
		if s.closed && len(s.control) == 0 && len(s.data) == 0 {
			s.mu.Unlock()
			return outboundJob{}, false
		}
		job, ok := s.pickLocked()
		if ok {
			s.mu.Unlock()
			return job, true
		}
		s.mu.Unlock()
		<-s.wake
	}
}

func (s *outboundScheduler) pickLocked() (outboundJob, bool) {
	// Anti-starvation: after ControlBurst consecutive control frames, admit one
	// data frame if any is waiting. The frame currently being written is never
	// preempted; priority only applies to not-yet-started frames.
	if s.controlBurst >= s.opts.ControlBurst && len(s.data) > 0 {
		return s.popDataLocked(), true
	}
	if len(s.control) > 0 {
		return s.popControlLocked(), true
	}
	if len(s.data) > 0 {
		return s.popDataLocked(), true
	}
	return outboundJob{}, false
}

func (s *outboundScheduler) popControlLocked() outboundJob {
	job := s.control[0]
	s.control = s.control[1:]
	s.controlBytes -= len(job.data)
	if s.controlBytes < 0 {
		s.controlBytes = 0
	}
	s.controlBurst++
	return job
}

func (s *outboundScheduler) popDataLocked() outboundJob {
	job := s.data[0]
	s.data = s.data[1:]
	s.dataBytes -= len(job.data)
	if s.dataBytes < 0 {
		s.dataBytes = 0
	}
	s.controlBurst = 0
	return job
}

func (s *outboundScheduler) stats() OutboundQueueStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return OutboundQueueStats{
		ControlFrames:    len(s.control),
		ControlBytes:     s.controlBytes,
		ControlHighWater: s.controlHW,
		DataFrames:       len(s.data),
		DataBytes:        s.dataBytes,
		DataHighWater:    s.dataHW,
		WaitedFrames:     s.waitedFrames.Load(),
		WaitedNanos:      s.waitedNanos.Load(),
		Overflows:        s.overflows.Load(),
	}
}
