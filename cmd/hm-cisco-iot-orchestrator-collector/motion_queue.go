package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const motionQueueLimit = 10000

// Only observations required for motion and battery persistence go to disk.
type motionObservation struct {
	Version    int `json:"version"`
	MAC        string
	ObservedAt time.Time
	ReceivedAt time.Time
	Motion     *bool
	Battery    *float64
	PacketID   *int
	APMAC      string
}

func motionFromReading(r bleReading) motionObservation {
	return motionObservation{Version: 1, MAC: r.SensorMAC, ObservedAt: r.TS, ReceivedAt: r.ReceivedAt, Motion: r.Motion, Battery: r.BatteryPercent, PacketID: r.PacketID, APMAC: r.APMAC}
}
func (o motionObservation) reading() bleReading {
	return bleReading{SensorMAC: o.MAC, TS: o.ObservedAt, ReceivedAt: o.ReceivedAt, Motion: o.Motion, BatteryPercent: o.Battery, PacketID: o.PacketID, APMAC: o.APMAC, SensorTypeCode: "shelly_blu_motion_zb"}
}

type motionQueue struct {
	mu         sync.Mutex
	dir        string
	lock       *os.File
	files      []string
	sequence   int64
	enqueueErr error
	wake       chan struct{}
}

func openMotionQueue(dir string) (*motionQueue, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("motion queue already in use: %w", err)
	}
	q := &motionQueue{dir: dir, lock: lock, sequence: time.Now().UnixNano(), wake: make(chan struct{}, 1)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		q.Close()
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			q.files = append(q.files, entry.Name())
		}
		// An interrupted temp file has never been acknowledged to the producer.
		if strings.HasPrefix(entry.Name(), ".pending-") {
			if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				q.Close()
				return nil, err
			}
		}
	}
	sort.Strings(q.files)
	if len(q.files) > 0 {
		var last int64
		if _, err = fmt.Sscanf(q.files[len(q.files)-1], "%d.json", &last); err != nil {
			q.Close()
			return nil, fmt.Errorf("invalid queue filename: %w", err)
		}
		if last > q.sequence {
			q.sequence = last
		}
	}
	return q, nil
}
func (q *motionQueue) Close() error { return q.lock.Close() }
func (q *motionQueue) syncDir() error {
	f, err := os.Open(q.dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (q *motionQueue) Push(r bleReading) (err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer func() { q.enqueueErr = err }()
	if len(q.files) >= motionQueueLimit {
		return errors.New("motion queue full (10000 observations)")
	}
	data, err := json.Marshal(motionFromReading(r))
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(q.dir, ".pending-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	q.sequence++
	name := fmt.Sprintf("%020d.json", q.sequence)
	if err = os.Rename(tmp, filepath.Join(q.dir, name)); err != nil {
		return err
	}
	// Keep renamed files in memory even when directory sync fails, so retries
	// cannot reorder later observations ahead of an uncertain durable write.
	q.files = append(q.files, name)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return q.syncDir()
}
func (q *motionQueue) Head() (string, bleReading, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.files) == 0 {
		return "", bleReading{}, nil
	}
	name := q.files[0]
	f, err := os.Open(filepath.Join(q.dir, name))
	if err != nil {
		return name, bleReading{}, err
	}
	defer f.Close()
	var o motionObservation
	if err = json.NewDecoder(f).Decode(&o); err != nil {
		return name, bleReading{}, fmt.Errorf("decode queued observation %s: %w", name, err)
	}
	if o.Version != 1 || o.MAC == "" || o.ObservedAt.IsZero() || o.ReceivedAt.IsZero() {
		return name, bleReading{}, errors.New("invalid queued observation")
	}
	return name, o.reading(), nil
}
func (q *motionQueue) Ack(name string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.files) == 0 || q.files[0] != name {
		return errors.New("motion queue acknowledgement out of order")
	}
	if err := os.Remove(filepath.Join(q.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	q.files = q.files[1:]
	return q.syncDir()
}
func (q *motionQueue) EnqueueError() error { q.mu.Lock(); defer q.mu.Unlock(); return q.enqueueErr }
func (q *motionQueue) Len() int            { q.mu.Lock(); defer q.mu.Unlock(); return len(q.files) }

func drainMotionQueue(ctx context.Context, q *motionQueue, c *collector) (int, error) {
	saved := 0
	for saved < 100 && ctx.Err() == nil {
		name, r, err := q.Head()
		if err != nil {
			return saved, err
		}
		if name == "" {
			return saved, nil
		}
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = c.storeMotion(writeCtx, r)
		cancel()
		if err != nil {
			return saved, err
		}
		if err = q.Ack(name); err != nil {
			return saved, err
		}
		saved++
	}
	return saved, ctx.Err()
}
func runMotionWriter(ctx context.Context, q *motionQueue, c *collector, reporter statusMarker) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		count, err := drainMotionQueue(ctx, q, c)
		err = errors.Join(err, q.EnqueueError())
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			reporter.MarkFailure(ctx, err)
			_, r, _ := q.Head()
			log.Printf("motion storage pending=%d oldest_age=%s: %v", q.Len(), time.Since(r.ReceivedAt).Round(time.Second), err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if count > 0 {
			reporter.MarkDataSuccess(ctx)
		} else if q.Len() == 0 {
			reporter.MarkSuccess(ctx)
		}
		if q.Len() > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-q.wake:
		}
	}
}
