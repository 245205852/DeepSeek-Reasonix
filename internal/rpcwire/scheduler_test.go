package rpcwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateWriter blocks the first Write until release is closed, then records
// subsequent payloads. Used to hold a data frame mid-write so later control
// frames can prove they overtake only the not-yet-started data queue.
type gateWriter struct {
	mu       sync.Mutex
	release  <-chan struct{}
	started  chan struct{}
	payloads []string
	err      error
}

func newGateWriter(release <-chan struct{}) *gateWriter {
	return &gateWriter{release: release, started: make(chan struct{})}
}

func (w *gateWriter) Write(p []byte) (int, error) {
	select {
	case <-w.started:
	default:
		close(w.started)
		if w.release != nil {
			<-w.release
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.payloads = append(w.payloads, string(bytes.TrimSpace(p)))
	return len(p), nil
}

func (w *gateWriter) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.payloads...)
}

func methodOf(line string) string {
	var frame struct {
		Method string `json:"method"`
		Result any    `json:"result"`
		Error  any    `json:"error"`
		ID     any    `json:"id"`
	}
	_ = json.Unmarshal([]byte(line), &frame)
	if frame.Method != "" {
		return frame.Method
	}
	if frame.Error != nil {
		return "error"
	}
	if frame.Result != nil {
		return "response"
	}
	return ""
}

func TestOutboundSchedulerControlOvertakesQueuedData(t *testing.T) {
	release := make(chan struct{})
	w := newGateWriter(release)
	queued := make(chan OutboundPriority, 3)
	classifier := func(meta OutboundMeta) OutboundPriority {
		if meta.Method == "broker/stream/chunk" || meta.Method == "data" {
			return PriorityData
		}
		return PriorityControl
	}
	conn := NewConn(strings.NewReader(""), w, Options{
		Name: "sched",
		Outbound: &OutboundSchedulerOptions{
			Classifier: classifier, ControlBurst: 16,
			ControlMaxFrames: 8, DataMaxFrames: 8,
			OnQueued: func(priority OutboundPriority) { queued <- priority },
		},
	})

	// First data write blocks inside the physical writer.
	firstDone := make(chan error, 1)
	go func() { firstDone <- conn.Notify("broker/stream/chunk", map[string]int{"n": 1}) }()
	select {
	case <-w.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first write did not start")
	}
	if got := <-queued; got != PriorityData {
		t.Fatalf("first queued priority = %v", got)
	}

	// Second data queues behind the in-flight frame.
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- conn.Notify("broker/stream/chunk", map[string]int{"n": 2})
	}()
	if got := <-queued; got != PriorityData {
		t.Fatalf("second queued priority = %v", got)
	}

	controlDone := make(chan error, 1)
	go func() { controlDone <- conn.Notify("remote/ping", map[string]any{}) }()
	if got := <-queued; got != PriorityControl {
		t.Fatalf("control queued priority = %v", got)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-controlDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}

	got := w.snapshot()
	if len(got) != 3 {
		t.Fatalf("writes = %v", got)
	}
	methods := []string{methodOf(got[0]), methodOf(got[1]), methodOf(got[2])}
	if methods[0] != "broker/stream/chunk" {
		t.Fatalf("first in-flight frame = %v", methods)
	}
	if methods[1] != "remote/ping" {
		t.Fatalf("control must overtake queued data: %v", methods)
	}
	if methods[2] != "broker/stream/chunk" {
		t.Fatalf("queued data must follow control: %v", methods)
	}
}

func TestOutboundSchedulerSameQueueFIFO(t *testing.T) {
	var buf bytes.Buffer
	conn := NewConn(strings.NewReader(""), &buf, Options{
		Outbound: RemoteOutboundScheduler(func(meta OutboundMeta) OutboundPriority {
			return PriorityData
		}),
	})
	var wg sync.WaitGroup
	// Serialize enqueue order with barriers so FIFO is deterministic.
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		n := i
		if err := conn.Notify("broker/stream/chunk", map[string]int{"n": n}); err != nil {
			t.Fatal(err)
		}
		wg.Done()
	}
	wg.Wait()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("lines = %d", len(lines))
	}
	for i, line := range lines {
		var frame struct {
			Params struct {
				N int `json:"n"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Params.N != i+1 {
			t.Fatalf("order[%d] = %d", i, frame.Params.N)
		}
	}
}

func TestOutboundSchedulerControlBurstAdmitsData(t *testing.T) {
	release := make(chan struct{})
	w := newGateWriter(release)
	queued := make(chan OutboundPriority, 4)
	conn := NewConn(strings.NewReader(""), w, Options{
		Outbound: &OutboundSchedulerOptions{
			Classifier: func(meta OutboundMeta) OutboundPriority {
				if meta.Method == "data" {
					return PriorityData
				}
				return PriorityControl
			},
			ControlBurst:     2,
			ControlMaxFrames: 16,
			DataMaxFrames:    8,
			OnQueued:         func(priority OutboundPriority) { queued <- priority },
		},
	})

	// Hold the first control write open.
	firstDone := make(chan error, 1)
	go func() { firstDone <- conn.Notify("ctrl", map[string]int{"n": 1}) }()
	<-w.started
	<-queued

	// Queue one more control and one data while the first is in flight.
	// After the first completes, control burst counter is 1; second control
	// makes burst 2; then data must be admitted before the third control.
	var orderMu sync.Mutex
	var finishOrder []string
	record := func(name string, err error) {
		orderMu.Lock()
		finishOrder = append(finishOrder, name)
		orderMu.Unlock()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	secondDone := make(chan struct{})
	go func() {
		err := conn.Notify("ctrl", map[string]int{"n": 2})
		record("c2", err)
		close(secondDone)
	}()
	if got := <-queued; got != PriorityControl {
		t.Fatalf("second control priority = %v", got)
	}
	dataDone := make(chan struct{})
	go func() {
		err := conn.Notify("data", map[string]int{"n": 1})
		record("d1", err)
		close(dataDone)
	}()
	if got := <-queued; got != PriorityData {
		t.Fatalf("data priority = %v", got)
	}
	thirdDone := make(chan struct{})
	go func() {
		err := conn.Notify("ctrl", map[string]int{"n": 3})
		record("c3", err)
		close(thirdDone)
	}()
	if got := <-queued; got != PriorityControl {
		t.Fatalf("third control priority = %v", got)
	}
	close(release)
	<-firstDone
	<-secondDone
	<-dataDone
	<-thirdDone

	got := w.snapshot()
	if len(got) != 4 {
		t.Fatalf("writes = %v", got)
	}
	methods := make([]string, len(got))
	for i, line := range got {
		methods[i] = methodOf(line)
	}
	// c1 (in flight), c2 (burst reaches 2), d1 (anti-starvation), c3
	want := []string{"ctrl", "ctrl", "data", "ctrl"}
	for i := range want {
		if methods[i] != want[i] {
			t.Fatalf("order = %v, want %v", methods, want)
		}
	}
}

func TestOutboundSchedulerOverflowClosesAndUnblocksWaiters(t *testing.T) {
	release := make(chan struct{})
	w := newGateWriter(release)
	queued := make(chan OutboundPriority, 2)
	conn := NewConn(strings.NewReader(""), w, Options{
		Outbound: &OutboundSchedulerOptions{
			Classifier:       func(OutboundMeta) OutboundPriority { return PriorityControl },
			ControlMaxFrames: 1,
			ControlMaxBytes:  1 << 20,
			ControlBurst:     16,
			OnQueued:         func(priority OutboundPriority) { queued <- priority },
		},
	})
	firstDone := make(chan error, 1)
	go func() { firstDone <- conn.Notify("hold", map[string]int{"n": 1}) }()
	<-w.started
	<-queued

	// One control is in flight; enqueue fills the only queue slot.
	secondDone := make(chan error, 1)
	go func() { secondDone <- conn.Notify("queued", map[string]int{"n": 2}) }()
	<-queued

	// Next enqueue overflows and fails the connection.
	err := conn.Notify("overflow", map[string]int{"n": 3})
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("overflow error = %v", err)
	}
	// Queued waiter must unblock with the same terminal error class.
	select {
	case err := <-secondDone:
		if err == nil {
			t.Fatal("queued waiter returned nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued waiter leaked")
	}
	close(release)
	<-firstDone
}

func TestOutboundSchedulerAfterWriteRunsOnceAfterPhysicalWrite(t *testing.T) {
	release := make(chan struct{})
	w := newGateWriter(release)
	conn := NewConn(
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"detach","params":{}}`+"\n"),
		w,
		Options{
			StrictJSONRPC: true,
			Outbound: RemoteOutboundScheduler(func(OutboundMeta) OutboundPriority {
				return PriorityControl
			}),
		},
	)
	callbacks := make(chan error, 2)
	conn.Handle("detach", func(context.Context, json.RawMessage) (any, error) {
		return RespondThen(map[string]bool{"detached": true}, func(err error) {
			callbacks <- err
		}), nil
	})
	done := make(chan error, 1)
	go func() { done <- conn.Serve(context.Background()) }()
	select {
	case <-w.started:
	case <-time.After(2 * time.Second):
		t.Fatal("response write never started")
	}
	select {
	case <-callbacks:
		t.Fatal("AfterWrite ran before physical write completed")
	default:
	}
	close(release)
	if err := <-done; err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		// Serve returns nil on clean EOF.
		if !strings.Contains(err.Error(), "EOF") {
			// Accept normal completion.
		}
	}
	select {
	case err := <-callbacks:
		if err != nil {
			t.Fatalf("AfterWrite error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AfterWrite never ran")
	}
	select {
	case <-callbacks:
		t.Fatal("AfterWrite ran more than once")
	default:
	}
}

func TestOutboundSchedulerCloseUnblocksConcurrentRequests(t *testing.T) {
	release := make(chan struct{})
	w := newGateWriter(release)
	queued := make(chan OutboundPriority, 9)
	// Keep the inbound side open so Serve does not EOF-close the connection
	// while we exercise the outbound fail path.
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	conn := NewConn(pr, w, Options{
		Outbound: RemoteOutboundScheduler(func(meta OutboundMeta) OutboundPriority {
			if strings.HasPrefix(meta.Method, "data") {
				return PriorityData
			}
			return PriorityControl
		}),
	})
	conn.scheduler.opts.OnQueued = func(priority OutboundPriority) { queued <- priority }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conn.Serve(ctx) }()

	holdDone := make(chan error, 1)
	go func() { holdDone <- conn.Notify("data", map[string]int{"n": 0}) }()
	select {
	case <-w.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first write never started")
	}
	<-queued

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- conn.Notify("data", map[string]int{"n": i + 1})
		}(i)
	}
	for i := 0; i < 8; i++ {
		if got := <-queued; got != PriorityData {
			t.Fatalf("queued priority[%d] = %v", i, got)
		}
	}
	conn.fail(errors.New("forced close"))
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("waiter returned nil after close")
		}
	}
	close(release)
	select {
	case <-holdDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight write never completed after release")
	}
}
