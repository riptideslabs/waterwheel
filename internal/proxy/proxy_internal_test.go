// SPDX-License-Identifier: Apache-2.0

package proxy

import "testing"

func TestSameTarget(t *testing.T) {
	t.Parallel()

	const api = "api.example.com"

	for _, tc := range []struct {
		reqHost string
		host    string
		port    int
		want    bool
	}{
		{api, api, 443, true},
		{api + ":443", api, 443, true},
		{"API.Example.com.", api, 443, true},
		{api + ":8443", api, 8443, true},
		{"[::1]", "::1", 443, true},
		{"[::1]:8443", "::1", 8443, true},
		{api, api, 8443, false},
		{api + ":444", api, 443, false},
		{api + ":", api, 443, false},
		{"other.example.com", api, 443, false},
		{api + ".other.example", api, 443, false},
		{"", api, 443, false},
	} {
		if got := sameTarget(tc.reqHost, tc.host, tc.port); got != tc.want {
			t.Errorf("sameTarget(%q, %q, %d) = %v; want %v", tc.reqHost, tc.host, tc.port, got, tc.want)
		}
	}
}
