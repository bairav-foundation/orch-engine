// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package egress parses the --egress-allow flag: a CIDR, optionally
// followed by ":" and comma-separated TCP ports.
//
//	10.20.0.0/16             all ports
//	203.0.113.7/32:443       one port
//	198.51.100.0/24:80,443   several ports
package egress

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Rule is one allowed destination.
type Rule struct {
	CIDR  string
	Ports []int32
}

// ParseAllow parses one --egress-allow value.
func ParseAllow(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	cidr, ports, hasPorts := strings.Cut(s, ":")
	if strings.Count(s, ":") > 1 {
		return Rule{}, fmt.Errorf("--egress-allow %q: IPv6 ranges aren't supported yet; use an IPv4 CIDR", s)
	}
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		hint := ""
		if net.ParseIP(cidr) != nil {
			hint = fmt.Sprintf(" (a single address is %s/32)", cidr)
		}
		return Rule{}, fmt.Errorf("--egress-allow %q: %q is not a CIDR%s", s, cidr, hint)
	}
	r := Rule{CIDR: cidr}
	if !hasPorts {
		return r, nil
	}
	for _, p := range strings.Split(ports, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 32)
		if err != nil || n < 1 || n > 65535 {
			return Rule{}, fmt.Errorf("--egress-allow %q: port %q must be 1-65535", s, p)
		}
		r.Ports = append(r.Ports, int32(n))
	}
	return r, nil
}

// ParseMode checks --egress.
func ParseMode(s string) (string, error) {
	switch s {
	case "", "open":
		return "open", nil
	case "restricted":
		return "restricted", nil
	}
	return "", fmt.Errorf("--egress %q: use open or restricted", s)
}
