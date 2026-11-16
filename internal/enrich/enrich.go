// Package enrich adds optional context from external CI services.
package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/sebrandon1/whyfail/internal/httpx"
)

const (
	SearchEndpoint = "https://search.dptools.openshift.org/search"
	SippyAPI       = "https://sippy.dptools.openshift.org/api/job/run/summary"
	userAgent      = "whyfail/dev (+https://github.com/sebrandon1/whyfail)"

	// maxRetries absorbs the transient 429/5xx these endpoints return under load.
	maxRetries = 2
)

// SearchResult summarizes how often a signature appears elsewhere.
type SearchResult struct {
	Query    string   `json:"query"`
	Matches  int      `json:"matches"`
	Jobs     []string `json:"jobs,omitempty"`
	KnownBug string   `json:"known_bug,omitempty"`
}

type searchMatch struct {
	Name    string   `json:"name"`
	Context []string `json:"context"`
}

var bugRe = regexp.MustCompile(`\b(?:OCPBUGS|OCPBUG|BZ)-\d+\b`)

// SearchCI queries search.ci for a regex and counts distinct jobs that match.
func SearchCI(ctx context.Context, hc *http.Client, query, maxAge string) (*SearchResult, error) {
	if maxAge == "" {
		maxAge = "48h"
	}
	q := url.Values{}
	q.Set("search", query)
	q.Set("maxAge", maxAge)
	q.Set("maxMatches", "3")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SearchEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpx.Do(hc, req, maxRetries)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search.ci: HTTP %d", resp.StatusCode)
	}
	var raw map[string]map[string][]searchMatch
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("search.ci: decode: %w", err)
	}
	res := &SearchResult{Query: query}
	for job, tests := range raw {
		res.Jobs = append(res.Jobs, job)
		for _, matches := range tests {
			for _, m := range matches {
				for _, c := range m.Context {
					if b := bugRe.FindString(c); b != "" && res.KnownBug == "" {
						res.KnownBug = b
					}
				}
			}
		}
	}
	res.Matches = len(res.Jobs)
	return res, nil
}

// SippyStatus probes whether Sippy knows about a run.
func SippyStatus(ctx context.Context, hc *http.Client, runID string) string {
	if runID == "" {
		return "unknown"
	}
	q := url.Values{}
	q.Set("prow_job_run_id", runID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SippyAPI+"?"+q.Encode(), nil)
	if err != nil {
		return "unknown"
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpx.Do(hc, req, maxRetries)
	if err != nil {
		return "unknown"
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	s := strings.ToLower(string(body))
	switch {
	case strings.Contains(s, "job run not found"):
		return "not tracked"
	case resp.StatusCode == http.StatusOK && strings.Contains(s, "{"):
		return "tracked"
	default:
		return "unknown"
	}
}
