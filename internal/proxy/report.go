// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.riptides.io/waterwheel/internal/bindings"
)

// refusedTries is how often a report the controller refuses is sent: a
// controller replica's cache may not have the pod's binding yet.
const refusedTries = 5

// Reporter sends reports to the controller in the background. It keeps the
// latest report about each binding, and about the pod's identity, until the
// controller takes it, so a pod doesn't keep a stale event because a report
// was lost. Events are best-effort, though: Report never blocks, and a report
// the controller keeps refusing is dropped.
type Reporter struct {
	backoff time.Duration

	mu      sync.Mutex
	pending []pending // oldest first, one per binding
	seq     uint64
	wake    chan struct{}
}

type pending struct {
	bindings.Report

	seq     uint64
	refused int
}

// NewReporter sends reports with send until ctx ends.
func NewReporter(ctx context.Context, send func(context.Context, bindings.Report) error) *Reporter {
	return newReporter(ctx, send, time.Second)
}

func newReporter(ctx context.Context, send func(context.Context, bindings.Report) error, backoff time.Duration) *Reporter {
	r := &Reporter{backoff: backoff, wake: make(chan struct{}, 1)}

	go r.run(ctx, send)

	return r
}

// Report queues rep, in place of any unsent report about the same binding.
func (r *Reporter) Report(rep bindings.Report) {
	r.mu.Lock()
	r.seq++
	r.pending = slices.DeleteFunc(r.pending, func(p pending) bool { return p.Binding == rep.Binding })
	r.pending = append(r.pending, pending{Report: rep, seq: r.seq})
	r.mu.Unlock()

	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Reporter) run(ctx context.Context, send func(context.Context, bindings.Report) error) {
	backoff := r.backoff

	for {
		next, ok := r.next()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			}

			continue
		}

		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := send(sctx, next.Report)

		cancel()

		r.done(next, err)

		if err == nil {
			backoff = r.backoff

			continue
		}

		slog.Info("report not sent", "reason", next.Reason, "binding", next.Binding, "err", err, "retryIn", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, time.Minute)
		}
	}
}

func (r *Reporter) next() (pending, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.pending) == 0 {
		return pending{}, false
	}

	return r.pending[0], true
}

// done takes p off the queue once sent, or dropped after too many refusals.
// Otherwise it goes to the back, so it doesn't hold up reports about other
// bindings. A newer report about the same binding has already replaced it.
func (r *Reporter) done(p pending, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	i := slices.IndexFunc(r.pending, func(q pending) bool { return q.seq == p.seq })
	if i < 0 {
		return
	}

	r.pending = slices.Delete(r.pending, i, i+1)

	if err == nil {
		return
	}

	if errors.Is(err, ErrReportRefused) {
		if p.refused++; p.refused >= refusedTries {
			slog.Warn("report dropped", "reason", p.Reason, "binding", p.Binding, "err", err)

			return
		}
	}

	r.pending = append(r.pending, p)
}
