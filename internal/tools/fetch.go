package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WebFetch retrieves a single URL from the HOST process (not the sandbox) and
// returns its readable text content. It is the internet-lookup companion to
// WebSearch, granted only to internet-allowed agents, and must never be pointed at
// the engagement target. Agents use this (and web_search) to read advisories, docs,
// and CVE pages instead of shelling out to curl/wget inside the sandbox.
type WebFetch struct {
	HTTP *http.Client
}

func (WebFetch) Name() string { return "web_fetch" }
func (WebFetch) Schema() Schema {
	return Schema{
		Name:        "web_fetch",
		Description: "Fetch a single http(s) URL from the internet and return its readable text (HTML tags stripped, truncated if large). Use this to read a specific page you already have the URL for — an advisory, a CVE entry, documentation, a changelog — rather than running curl/wget. It fetches from the host, outside the sandbox, so it CANNOT reach the engagement target or any internal host; never point it at the target. Pair it with web_search, which finds URLs.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"the absolute http(s) URL to fetch"},"max_chars":{"type":"integer","description":"max characters of text to return (default 20000)"}},"required":["url"]}`),
	}
}

// webFetchDefault is how much decoded text a fetch returns by default, and
// webFetchHardMax the ceiling, so a huge page can't flood the agent's context.
const webFetchDefault = 20000
const webFetchHardMax = 200000

func (wf WebFetch) Execute(ctx context.Context, c Call) (Result, error) {
	var a struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil || strings.TrimSpace(a.URL) == "" {
		return Result{Content: "web_fetch: 'url' is required", IsError: true}, nil
	}
	u := strings.TrimSpace(a.URL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return Result{Content: "web_fetch: url must be an absolute http(s) URL", IsError: true}, nil
	}
	limit := a.MaxChars
	if limit <= 0 {
		limit = webFetchDefault
	}
	if limit > webFetchHardMax {
		limit = webFetchHardMax
	}

	client := wf.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{Content: "web_fetch: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; oline-researcher/0.1)")
	resp, err := client.Do(req)
	if err != nil {
		return Result{Content: "web_fetch: request failed: " + err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()
	// Read a bounded amount of the body: enough raw bytes to yield `limit` characters
	// of text after tag-stripping, but never unbounded.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, int64(limit)*8+1<<16))

	text := cleanHTML(string(raw))
	text = collapseBlankLines(text)
	truncated := false
	if r := []rune(text); len(r) > limit {
		text = string(r[:limit])
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n\n%s", u, resp.Status, text)
	if truncated {
		b.WriteString("\n\n[truncated — refetch with a larger max_chars for more]")
	}
	return Result{Content: b.String(), IsError: resp.StatusCode >= 400}, nil
}

// collapseBlankLines squeezes runs of blank lines (left behind by tag stripping)
// down to a single blank line so the returned text stays compact.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, strings.TrimRight(ln, " \t"))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
