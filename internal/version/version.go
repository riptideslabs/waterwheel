// SPDX-License-Identifier: Apache-2.0

// Package version reports the version a binary was built from.
package version

import "runtime/debug"

// Version is the Waterwheel release the binary was built from, such as v0.1.0,
// as go build records it from the Git tag. Other builds get a pseudo-version,
// with +dirty for uncommitted changes.
func Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}

	return "unknown"
}
