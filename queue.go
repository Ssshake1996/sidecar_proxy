package main

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

type RecordQueue struct {
	items   chan CaptureRecord
	dropped atomic.Uint64
	mu      sync.RWMutex
	closed  bool
}

func NewRecordQueue(size int) *RecordQueue {
	return &RecordQueue{items: make(chan CaptureRecord, size)}
}

func (q *RecordQueue) Enqueue(record CaptureRecord) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		q.dropped.Add(1)
		return false
	}
	select {
	case q.items <- record:
		return true
	default:
		q.dropped.Add(1)
		return false
	}
}

func (q *RecordQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.items)
	}
}

func (q *RecordQueue) Dropped() uint64 {
	if q == nil {
		return 0
	}
	return q.dropped.Load()
}

func (q *RecordQueue) Run(ctx context.Context, store *Store, resolver *IdentityResolver, cfg Config) {
	for record := range q.items {
		if ctx.Err() != nil {
			return
		}
		q.persist(ctx, store, resolver, cfg, record)
	}
}

func (q *RecordQueue) persist(ctx context.Context, store *Store, resolver *IdentityResolver, cfg Config, record CaptureRecord) {
	if resolver != nil && !record.Identity.Resolved() {
		for attempt := 0; attempt < 3; attempt++ {
			lookupCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
			if identity, ok := resolver.ResolveRequestID(lookupCtx, record.RequestID); ok {
				record.Identity = identity
				cancel()
				break
			}
			cancel()
			if attempt < 2 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
	}
	if !record.Identity.Resolved() && !cfg.AllowUnknownIdentity {
		return
	}
	if record.MessageCount == 0 && !cfg.CaptureEmptyPrompts {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := store.Insert(writeCtx, record)
		cancel()
		if err == nil {
			return
		}
		if attempt == 2 {
			log.Printf("prompt-audit insert failed: %v", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		}
	}
}
