// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func sampleReports() []*Report {
	return []*Report{
		{Suite: "basics", Target: "http://a/v1", Started: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), Seconds: 1.5,
			Passed: 1, Total: 2, Score: 0.5, Threshold: 0.8,
			Cases: []CaseResult{
				{Name: "ok", Pass: true, Passed: 1, Repeat: 1, MinPass: 1, Attempts: []Attempt{{Reply: "OK", Status: 200, LatencyMS: 200}}},
				{Name: "json | pipe", Passed: 0, Repeat: 1, MinPass: 1, Attempts: []Attempt{{Reply: "<not json>", Status: 200, LatencyMS: 900, Failures: []string{"reply is not valid JSON"}}}},
			}},
		{Suite: "dlp", Target: "http://proxy/v1", Seconds: 0.4, Passed: 1, Total: 1, Score: 1, Threshold: 1, Pass: true,
			Cases: []CaseResult{{Name: "email-redacted", Pass: true, Passed: 1, Repeat: 1, MinPass: 1, Attempts: []Attempt{{Status: 200, LatencyMS: 400}}}}},
	}
}

func TestWriteJUnit(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJUnit(&buf, sampleReports()); err != nil {
		t.Fatal(err)
	}
	var doc junitSuites
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("not valid XML: %v\n%s", err, buf.String())
	}
	if doc.Tests != 3 || doc.Failures != 1 || len(doc.Suites) != 2 {
		t.Fatalf("tests %d failures %d suites %d", doc.Tests, doc.Failures, len(doc.Suites))
	}
	c := doc.Suites[0].Cases[1]
	if c.Failure == nil || !strings.Contains(c.Failure.Message, "not valid JSON") || !strings.Contains(c.Failure.Text, "<not json>") {
		t.Fatalf("failure not reported: %+v", c.Failure)
	}
	if doc.Suites[0].Cases[0].Failure != nil || c.Time != "0.900" {
		t.Fatalf("passing case has a failure, or time %q", c.Time)
	}
}

func TestWriteMarkdown(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, sampleReports()); err != nil {
		t.Fatal(err)
	}
	md := buf.String()
	for _, want := range []string{"| basics | `http://a/v1` | 1/2 (50%) | 80% | ❌ fail |", "| dlp |", "✅ pass", `json \| pipe`, "reply is not valid JSON"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "**dlp** failed") {
		t.Error("a passing suite should not list failed cases")
	}
}
