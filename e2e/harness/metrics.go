//go:build e2e

package harness

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MetricsDump is a parsed Prometheus text scrape: every sample line as
// name + labels + value, plus the raw text for the artifact.
type MetricsDump struct {
	Raw     string
	Samples []Sample
}

// Sample is one Prometheus exposition sample line.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// ScrapeMetrics GETs the Prometheus endpoint and parses the text exposition
// format (the subset the assertions need: NAME{LABELS} VALUE lines).
func ScrapeMetrics(url string) (*MetricsDump, error) {
	httpc := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpc.Get(url)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	dump := &MetricsDump{Raw: string(body)}
	for _, line := range strings.Split(dump.Raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if s, ok := parseSample(line); ok {
			dump.Samples = append(dump.Samples, s)
		}
	}
	return dump, nil
}

// parseSample parses `name{l1="v1",l2="v2"} value [timestamp]`.
func parseSample(line string) (Sample, bool) {
	s := Sample{Labels: map[string]string{}}
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		s.Name = line[:i]
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return s, false
		}
		if !parseLabels(line[i+1:j], s.Labels) {
			return s, false
		}
		rest = strings.TrimSpace(line[j+1:])
	} else {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return s, false
		}
		s.Name = fields[0]
		rest = fields[1]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return s, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, false
	}
	s.Value = v
	return s, true
}

// parseLabels parses `k="v",k2="v2"` (quoted values; escaped quotes handled
// well enough for the bounded label sets mecatl emits).
func parseLabels(in string, out map[string]string) bool {
	for in != "" {
		eq := strings.IndexByte(in, '=')
		if eq < 0 {
			return false
		}
		key := strings.TrimSpace(in[:eq])
		rest := in[eq+1:]
		if len(rest) == 0 || rest[0] != '"' {
			return false
		}
		end := 1
		for end < len(rest) {
			if rest[end] == '\\' {
				end += 2
				continue
			}
			if rest[end] == '"' {
				break
			}
			end++
		}
		if end >= len(rest) {
			return false
		}
		out[key] = strings.ReplaceAll(rest[1:end], `\"`, `"`)
		in = strings.TrimPrefix(strings.TrimSpace(rest[end+1:]), ",")
		in = strings.TrimSpace(in)
	}
	return true
}

// Sum adds every sample whose name matches and whose labels are a superset of
// match.
func (d *MetricsDump) Sum(name string, match map[string]string) float64 {
	var total float64
	for _, s := range d.Samples {
		if s.Name != name || !labelsMatch(s.Labels, match) {
			continue
		}
		total += s.Value
	}
	return total
}

// Has reports whether at least one sample matches name + labels.
func (d *MetricsDump) Has(name string, match map[string]string) bool {
	for _, s := range d.Samples {
		if s.Name == name && labelsMatch(s.Labels, match) {
			return true
		}
	}
	return false
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
