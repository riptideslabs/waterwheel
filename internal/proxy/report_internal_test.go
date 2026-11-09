// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.riptides.io/waterwheel/internal/bindings"
)

// gated is a Reporter whose sends each wait for the test to release them.
type gated struct {
	*Reporter

	tried   chan string
	release chan struct{}
}

// gatedReporter's sends go as send says, once released.
func gatedReporter(t *testing.T, send func(bindings.Report) error) *gated {
	t.Helper()

	g := &gated{tried: make(chan string), release: make(chan struct{})}
	g.Reporter = newReporter(t.Context(), func(ctx context.Context, rep bindings.Report) error {
		select {
		case g.tried <- rep.Binding:
		case <-ctx.Done():
			return ctx.Err()
		}

		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}

		return send(rep)
	}, time.Millisecond)

	return g
}

// tries expects the next send to be about binding, and holds it up.
func (g *gated) tries(t *testing.T, binding string) {
	t.Helper()

	select {
	case got := <-g.tried:
		if got != binding {
			t.Fatalf("sent a report about %s; want %s", got, binding)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no report about %s sent", binding)
	}
}

// sends expects sends about each binding in turn, and lets each finish.
func (g *gated) sends(t *testing.T, bindings ...string) {
	t.Helper()

	for _, b := range bindings {
		g.tries(t, b)
		g.release <- struct{}{}
	}
}

func issued(binding string) bindings.Report {
	return bindings.Report{Binding: binding, Reason: bindings.ReasonCredentialIssued}
}

// TestReporterKeepsTheLatest: a newer report about a binding replaces one
// that isn't sent yet.
func TestReporterKeepsTheLatest(t *testing.T) {
	t.Parallel()

	r := gatedReporter(t, func(bindings.Report) error { return nil })

	r.Report(issued("a"))
	r.tries(t, "a")

	r.Report(issued("b"))
	r.Report(bindings.Report{Binding: "a", Reason: bindings.ReasonTokenExchangeFailed})
	r.Report(issued("a"))
	r.release <- struct{}{}

	r.sends(t, "b", "a")

	r.Report(issued("c"))
	r.sends(t, "c")
}

// TestReporterRetries: a report that doesn't get through is sent again,
// however long the controller is away.
func TestReporterRetries(t *testing.T) {
	t.Parallel()

	const failures = refusedTries + 1

	left := failures
	r := gatedReporter(t, func(bindings.Report) error {
		if left > 0 {
			left--

			return errTimeout
		}

		return nil
	})

	r.Report(issued("a"))

	for range failures + 1 {
		r.sends(t, "a")
	}

	r.Report(issued("b"))
	r.sends(t, "b")
}

// TestReporterDropsRefused: a report the controller keeps refusing is
// dropped, and doesn't hold up others meanwhile.
func TestReporterDropsRefused(t *testing.T) {
	t.Parallel()

	r := gatedReporter(t, func(rep bindings.Report) error {
		if rep.Binding == "refused" {
			return fmt.Errorf("%w: 403 Forbidden", ErrReportRefused)
		}

		return nil
	})

	r.Report(issued("refused"))
	r.tries(t, "refused")

	r.Report(issued("a"))
	r.release <- struct{}{}

	r.sends(t, "a")

	for range refusedTries - 2 {
		r.sends(t, "refused")
	}

	r.tries(t, "refused") // the last try
	r.Report(issued("b"))
	r.release <- struct{}{}

	r.sends(t, "b")

	r.Report(issued("c"))
	r.sends(t, "c")
}
