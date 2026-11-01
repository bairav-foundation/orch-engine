// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

// Built-in suites ship inside the binary so a team can check a model
// without writing any YAML: `bairav eval run --builtin basics,json`.
// To change one, print it (`bairav eval init --from basics`) and edit the copy.
//
//go:embed builtin/*.yaml
var builtinFS embed.FS

// BuiltinPrefix marks a built-in suite where a file path is also accepted,
// e.g. `bairav model upgrade ... --eval builtin:basics`.
const BuiltinPrefix = "builtin:"

// BuiltinNames lists the built-in suites, sorted.
func BuiltinNames() []string {
	entries, _ := builtinFS.ReadDir("builtin")
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// BuiltinYAML returns the source of a built-in suite.
func BuiltinYAML(name string) ([]byte, error) {
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), BuiltinPrefix)
	b, err := builtinFS.ReadFile("builtin/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("no built-in suite %q (have: %s)", name, strings.Join(BuiltinNames(), ", "))
	}
	return b, nil
}

// LoadBuiltin parses a built-in suite.
func LoadBuiltin(name string) (*Suite, error) {
	b, err := BuiltinYAML(name)
	if err != nil {
		return nil, err
	}
	return Parse(b, BuiltinPrefix+strings.TrimPrefix(name, BuiltinPrefix))
}

// Resolve loads "builtin:NAME" from the binary and anything else from disk.
func Resolve(ref string) (*Suite, error) {
	if strings.HasPrefix(ref, BuiltinPrefix) {
		return LoadBuiltin(ref)
	}
	return Load(ref)
}
