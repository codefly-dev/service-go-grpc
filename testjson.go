package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// testEvent represents one line of `go test -json` output.
type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// testSummary holds the parsed results of a `go test -json` run.
type testSummary struct {
	Run      int32
	Passed   int32
	Failed   int32
	Skipped  int32
	Coverage float32
	Failures []string

	failOutput map[string]*strings.Builder
}

var coverageRe = regexp.MustCompile(`coverage:\s+([\d.]+)%`)

// parseTestJSON parses the accumulated output of `go test -json -cover`.
func parseTestJSON(raw string) *testSummary {
	s := &testSummary{
		failOutput: make(map[string]*strings.Builder),
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev testEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}

		switch ev.Action {
		case "pass":
			if ev.Test != "" {
				s.Run++
				s.Passed++
			}
		case "fail":
			if ev.Test != "" {
				s.Run++
				s.Failed++
				key := ev.Package + "/" + ev.Test
				if buf, ok := s.failOutput[key]; ok {
					s.Failures = append(s.Failures, fmt.Sprintf("FAIL %s\n%s", key, buf.String()))
				} else {
					s.Failures = append(s.Failures, fmt.Sprintf("FAIL %s", key))
				}
			}
		case "skip":
			if ev.Test != "" {
				s.Run++
				s.Skipped++
			}
		case "output":
			if m := coverageRe.FindStringSubmatch(ev.Output); len(m) > 1 {
				var pct float64
				fmt.Sscanf(m[1], "%f", &pct)
				if float32(pct) > s.Coverage {
					s.Coverage = float32(pct)
				}
			}
			if ev.Test != "" {
				key := ev.Package + "/" + ev.Test
				if _, ok := s.failOutput[key]; !ok {
					s.failOutput[key] = &strings.Builder{}
				}
				s.failOutput[key].WriteString(ev.Output)
			}
		}
	}

	return s
}

// summaryLine formats a one-line summary string.
func (s *testSummary) summaryLine() string {
	parts := []string{fmt.Sprintf("%d passed", s.Passed)}
	if s.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", s.Failed))
	}
	if s.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", s.Skipped))
	}
	if s.Coverage > 0 {
		parts = append(parts, fmt.Sprintf("%.1f%% coverage", s.Coverage))
	}
	return strings.Join(parts, ", ")
}

// lineCapture implements io.Writer and accumulates all written data
// with newlines preserved (the native runner strips trailing whitespace).
type lineCapture struct {
	buf strings.Builder
}

func (lc *lineCapture) Write(p []byte) (n int, err error) {
	lc.buf.Write(p)
	lc.buf.WriteByte('\n')
	return len(p), nil
}

func (lc *lineCapture) String() string {
	return lc.buf.String()
}
