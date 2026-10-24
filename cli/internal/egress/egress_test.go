// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAllow(t *testing.T) {
	ok := map[string]Rule{
		"10.20.0.0/16":           {CIDR: "10.20.0.0/16"},
		" 203.0.113.7/32:443 ":   {CIDR: "203.0.113.7/32", Ports: []int32{443}},
		"198.51.100.0/24:80,443": {CIDR: "198.51.100.0/24", Ports: []int32{80, 443}},
	}
	for in, want := range ok {
		got, err := ParseAllow(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseAllow(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	bad := map[string]string{
		"203.0.113.7":         "/32",
		"example.com":         "not a CIDR",
		"10.0.0.0/8:0":        "1-65535",
		"10.0.0.0/8:https":    "1-65535",
		"10.0.0.0/8:443,":     "1-65535",
		"2001:db8::/32":       "IPv6",
		"10.0.0.0/8:443:8443": "IPv6",
	}
	for in, want := range bad {
		if _, err := ParseAllow(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseAllow(%q) err = %v, want it to mention %q", in, err, want)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]string{"": "open", "open": "open", "restricted": "restricted"} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("closed"); err == nil {
		t.Error("ParseMode(closed) should fail")
	}
}
