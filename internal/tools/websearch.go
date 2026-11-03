package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// WebSearch performs an internet search from the HOST process (not the sandbox).
// It is the internet-lookup capability granted only to internet-allowed agents
// (researcher / installer / judge / skeptic). It must never be pointed at the
// engagement target. Because it runs in the host process, it is outside the
// sandbox's per-agent egress; the in-guest "internet-minus-target" routing
// governs commands run inside the sandbox, not this host-side call.
type WebSearch struct {
	HTTP *http.Client
}

func (WebSearch) Name() string { return "web_search" }
func (WebSearch) Schema() Schema {
	return Schema{
		Name:        "web_search",
		Description: "Search the web for information (CVEs, advisories, techniques, package/download locations). Returns ranked result titles, URLs, and snippets. Never used against the engagement target.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"max":{"type":"integer","description":"max results (default 5)"}},"required":["query"]}`),
	}
}

var (
	// reResultAnchor matches any anchor whose href is a DuckDuckGo result redirect
	// (it carries a `uddg=` target). Keying on that redirect — the stable signal
	// across the html and lite front ends — instead of a specific CSS class makes
	// parsing resilient to DuckDuckGo's frequent markup/class changes.
	reResultAnchor = regexp.MustCompile(`(?is)<a[^>]+href="([^"]*[?&]uddg=[^"]*)"[^>]*>(.*?)</a>`)
	reTag          = regexp.MustCompile(`(?s)<[^>]+>`)
)

// ddgEndpoints are tried in order; both encode result links as `/l/?uddg=<url>`.
// If the first front end changes or returns nothing parseable, the next is tried.
var ddgEndpoints = []string{
	"https://html.duckduckgo.com/html/?q=",
	"https://lite.duckduckgo.com/lite/?q=",
}

func (ws WebSearch) Execute(ctx context.Context, c Call) (Result, error) {
	var a struct {
		Query string `json:"query"`
		Max   int    `json:"max"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil || strings.TrimSpace(a.Query) == "" {
		return Result{Content: "web_search: 'query' is required", IsError: true}, nil
	}
	if a.Max <= 0 || a.Max > 20 {
		a.Max = 5
	}
	client := ws.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	var lastStatus, lastErr string
	for _, base := range ddgEndpoints {
		results, status, err := ddgFetch(ctx, client, base+url.QueryEscape(a.Query), a.Max)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		lastStatus = status
		if len(results) == 0 {
			continue // front end reachable but nothing parsed; try the next one
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Results for %q:\n", a.Query)
		for i, r := range results {
			fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.title, r.url)
		}
		return Result{Content: strings.TrimRight(b.String(), "\n")}, nil
	}
	if lastErr != "" && lastStatus == "" {
		return Result{Content: "web_search: request failed: " + lastErr, IsError: true}, nil
	}
	return Result{Content: fmt.Sprintf("web_search: no parseable results for %q (last status %s)", a.Query, lastStatus)}, nil
}

type ddgResult struct{ title, url string }

// ddgFetch retrieves one DuckDuckGo front end and extracts up to max de-duplicated
// result links (decoded from their `uddg=` redirect), with the anchor text as title.
func ddgFetch(ctx context.Context, client *http.Client, endpoint string, max int) ([]ddgResult, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; oline-researcher/0.1)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out []ddgResult
	seen := map[string]bool{}
	for _, m := range reResultAnchor.FindAllStringSubmatch(string(body), -1) {
		link := decodeDDG(m[1])
		if link == "" || seen[link] {
			continue
		}
		title := cleanHTML(m[2])
		if title == "" {
			title = link
		}
		seen[link] = true
		out = append(out, ddgResult{title: title, url: link})
		if len(out) >= max {
			break
		}
	}
	return out, resp.Status, nil
}

func cleanHTML(s string) string {
	s = reTag.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&#x27;", "'")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.TrimSpace(s)
}

// decodeDDG unwraps DuckDuckGo's redirect links (//duckduckgo.com/l/?uddg=...).
func decodeDDG(href string) string {
	if strings.Contains(href, "uddg=") {
		if u, err := url.Parse(href); err == nil {
			if real := u.Query().Get("uddg"); real != "" {
				return real
			}
		}
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return href
}
