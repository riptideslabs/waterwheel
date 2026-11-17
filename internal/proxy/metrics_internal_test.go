// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"testing"
)

// other is the OpenTelemetry conventions' value for one they don't know.
const other = "_OTHER"

func TestMethodAttr(t *testing.T) {
	t.Parallel()

	for method, want := range map[string]string{
		"GET": "GET", "POST": "POST", "QUERY": "QUERY", "CONNECT": "CONNECT",
		"get": other, "BREW": other, "": other,
	} {
		if got := methodAttr(method).Value.AsString(); got != want {
			t.Errorf("%q is %q; want %q", method, got, want)
		}
	}
}

func TestErrorTypeOfAnAbortedResponse(t *testing.T) {
	t.Parallel()

	if got := errorType(fmt.Errorf("copying: %w", errAborted)); got != "aborted" {
		t.Errorf("got %q; want aborted", got)
	}
}
