// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"log/slog"
	"time"

	"go.riptides.io/waterwheel/internal/bindings"
)

// Reporter sends reports to the controller in the background, in order. It
// drops them when it falls behind: events are best-effort, and must never
// hold up a request.
type Reporter struct {
	ch chan bindings.Report
}

// NewReporter sends reports with send until ctx ends.
func NewReporter(ctx context.Context, send func(context.Context, bindings.Report) error) *Reporter {
	r := &Reporter{ch: make(chan bindings.Report, 32)}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case rep := <-r.ch:
				sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := send(sctx, rep); err != nil {
					slog.Info("report not sent", "reason", rep.Reason, "binding", rep.Binding, "err", err)
				}

				cancel()
			}
		}
	}()

	return r
}

// Report queues rep, or drops it when the queue is full.
func (r *Reporter) Report(rep bindings.Report) {
	select {
	case r.ch <- rep:
	default:
	}
}
