package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func queuedReading(motion bool, ts time.Time) bleReading {
	return bleReading{SensorTypeCode: "shelly_blu_motion_zb", SensorMAC: "fc:4d:6a:26:53:e3", TS: ts, ReceivedAt: ts, Motion: &motion, BatteryPercent: floatPtr(100)}
}
func TestMotionQueueRetryAndRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := openMotionQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC()
	for i, state := range []bool{false, true, false, true} {
		if err = q.Push(queuedReading(state, ts.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = openMotionQueue(dir); err == nil {
		t.Fatal("allowed concurrent queue owner")
	}
	db := &motionTestDB{err: errors.New("DB unavailable")}
	c := &collector{db: db}
	if n, err := drainMotionQueue(context.Background(), q, c); err == nil || n != 0 || q.Len() != 4 {
		t.Fatalf("lost pending observations n=%d err=%v remaining=%d", n, err, q.Len())
	}
	q.Close()
	q, err = openMotionQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i, state := range []bool{false, true, false, true} {
		name, r, err := q.Head()
		if err != nil || r.Motion == nil || *r.Motion != state || !r.TS.Equal(ts.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("recovery changed observation %+v err=%v", r, err)
		}
		if err = q.Ack(name); err != nil {
			t.Fatal(err)
		}
	}
	if q.Len() != 0 {
		t.Fatal("queue not drained")
	}
}
func TestMotionQueueSuccessAndCapacity(t *testing.T) {
	q, err := openMotionQueue(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err = q.Push(queuedReading(true, time.Now())); err != nil {
		t.Fatal(err)
	}
	db := &motionTestDB{}
	if n, err := drainMotionQueue(context.Background(), q, &collector{db: db}); err != nil || n != 1 || q.Len() != 0 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	q.files = make([]string, motionQueueLimit)
	if err = q.Push(queuedReading(false, time.Now())); err == nil || q.EnqueueError() == nil {
		t.Fatal("queue capacity error hidden")
	}
}
func TestMotionQueueCorruptHeadIsPreserved(t *testing.T) {
	dir := t.TempDir()
	q, err := openMotionQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err = q.Push(queuedReading(true, time.Now())); err != nil {
		t.Fatal(err)
	}
	name, _, _ := q.Head()
	if err = os.WriteFile(filepath.Join(dir, name), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := drainMotionQueue(context.Background(), q, &collector{db: &motionTestDB{}}); err == nil || n != 0 || q.Len() != 1 {
		t.Fatal("corrupt observation silently discarded")
	}
}

type cancelStatusMarker struct {
	recordingStatusMarker
	cancel context.CancelFunc
}

func (r *cancelStatusMarker) MarkFailure(ctx context.Context, err error) {
	r.recordingStatusMarker.MarkFailure(ctx, err)
	r.cancel()
}
func TestMotionWriterFailureCannotBecomeNetworkSuccess(t *testing.T) {
	q, err := openMotionQueue(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err = q.Push(queuedReading(true, time.Now())); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reporter := &cancelStatusMarker{cancel: cancel}
	runMotionWriter(ctx, q, &collector{db: &motionTestDB{err: errors.New("DB unavailable")}}, reporter)
	if len(reporter.calls) != 1 || reporter.calls[0] != "failure" || q.Len() != 1 {
		t.Fatalf("failure hidden calls=%v pending=%d", reporter.calls, q.Len())
	}
}
