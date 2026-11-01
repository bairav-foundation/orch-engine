// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// JUnit XML, the format CI systems (GitHub Actions test reporters, GitLab,
// Jenkins) show as a test list: one <testsuite> per suite and endpoint, one
// <testcase> per case.

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	Time      string          `xml:"time,attr"`
	Timestamp string          `xml:"timestamp,attr"`
	Props     []junitProperty `xml:"properties>property"`
	Cases     []junitCase     `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Class   string        `xml:"classname,attr"`
	Name    string        `xml:"name,attr"`
	Time    string        `xml:"time,attr"`
	Failure *junitFailure `xml:"failure,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// WriteJUnit writes the reports as one JUnit XML document.
func WriteJUnit(w io.Writer, reports []*Report) error {
	doc := junitSuites{Name: "bairav eval"}
	var total float64
	for _, r := range reports {
		s := junitSuite{
			Name:      r.Suite + " @ " + r.Target,
			Tests:     r.Total,
			Failures:  r.Total - r.Passed,
			Time:      secs(r.Seconds),
			Timestamp: r.Started.UTC().Format("2006-01-02T15:04:05"),
			Props: []junitProperty{
				{Name: "target", Value: r.Target},
				{Name: "score", Value: fmt.Sprintf("%.2f", r.Score)},
				{Name: "threshold", Value: fmt.Sprintf("%.2f", r.Threshold)},
				{Name: "pass", Value: fmt.Sprint(r.Pass)},
			},
		}
		for _, c := range r.Cases {
			var ms int64
			for _, a := range c.Attempts {
				ms += a.LatencyMS
			}
			jc := junitCase{Class: r.Suite, Name: c.Name, Time: secs(float64(ms) / 1000)}
			if !c.Pass {
				jc.Failure = &junitFailure{
					Message: fmt.Sprintf("%d/%d attempts passed, needs %d: %s", c.Passed, c.Repeat, c.MinPass, strings.Join(firstFailed(c), "; ")),
					Text:    attemptLog(c),
				}
			}
			s.Cases = append(s.Cases, jc)
		}
		doc.Tests += s.Tests
		doc.Failures += s.Failures
		total += r.Seconds
		doc.Suites = append(doc.Suites, s)
	}
	doc.Time = secs(total)
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// WriteMarkdown writes a short summary table plus the failed cases. Point
// it at $GITHUB_STEP_SUMMARY to show results on the Actions run page.
func WriteMarkdown(w io.Writer, reports []*Report) error {
	var b strings.Builder
	b.WriteString("### bairav eval\n\n")
	b.WriteString("| suite | endpoint | score | needs | result |\n|---|---|---|---|---|\n")
	for _, r := range reports {
		verdict := "✅ pass"
		if !r.Pass {
			verdict = "❌ fail"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %d/%d (%.0f%%) | %.0f%% | %s |\n",
			mdCell(r.Suite), mdCell(r.Target), r.Passed, r.Total, r.Score*100, r.Threshold*100, verdict)
	}
	for _, r := range reports {
		var failed []CaseResult
		for _, c := range r.Cases {
			if !c.Pass {
				failed = append(failed, c)
			}
		}
		if len(failed) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n**%s** failed cases (`%s`):\n\n| case | passed | why |\n|---|---|---|\n", mdCell(r.Suite), mdCell(r.Target))
		for _, c := range failed {
			fmt.Fprintf(&b, "| %s | %d/%d | %s |\n", mdCell(c.Name), c.Passed, c.Repeat, mdCell(clip(strings.Join(firstFailed(c), "; "), 160)))
		}
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// firstFailed returns the failed checks of the first failing attempt.
func firstFailed(c CaseResult) []string {
	for _, a := range c.Attempts {
		if !a.Passed() {
			return a.Failures
		}
	}
	return nil
}

func attemptLog(c CaseResult) string {
	var b strings.Builder
	for i, a := range c.Attempts {
		status := "pass"
		if !a.Passed() {
			status = "fail: " + strings.Join(a.Failures, "; ")
		}
		fmt.Fprintf(&b, "attempt %d (HTTP %d, %d ms) %s\nreply: %s\n", i+1, a.Status, a.LatencyMS, status, clip(a.Reply, 300))
	}
	return b.String()
}

func secs(s float64) string { return fmt.Sprintf("%.3f", s) }

// mdCell keeps a value inside one markdown table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.Join(strings.Fields(s), " ")
}
