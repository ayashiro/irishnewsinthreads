package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

const (
	baseURL             = "https://generativelanguage.googleapis.com/v1beta/models"
	maxThreadsPostRunes = 500
)

// Source represents a web grounding source returned by Gemini Google Search grounding.
type Source struct {
	Title string `json:"title"`
	URI   string `json:"uri"`
}

// NewsResult contains the generated Threads post text and its web sources.
type NewsResult struct {
	PostText string   `json:"post_text"`
	Sources  []Source `json:"sources,omitempty"`
}

// Client calls the Gemini REST API with Google Search grounding enabled.
type Client struct {
	apiKey     string
	model      string
	promptsDir string
	httpClient *http.Client
}

// NewClient creates a new Gemini REST API client.
func NewClient(apiKey, model, promptsDir string) *Client {
	if strings.TrimSpace(promptsDir) == "" {
		promptsDir = "prompts"
	}
	return &Client{
		apiKey:     apiKey,
		model:      model,
		promptsDir: promptsDir,
		httpClient: &http.Client{
			Timeout: 150 * time.Second,
		},
	}
}

type generateRequest struct {
	Contents []content `json:"contents"`
	Tools    []tool    `json:"tools,omitempty"`
}

type content struct {
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type googleSearchTool struct{}

type tool struct {
	GoogleSearch *googleSearchTool `json:"google_search,omitempty"`
}

type generateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		GroundingMetadata struct {
			GroundingChunks []struct {
				Web *struct {
					URI   string `json:"uri"`
					Title string `json:"title"`
				} `json:"web"`
			} `json:"groundingChunks"`
		} `json:"groundingMetadata"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

type promptData struct {
	Date  string
	Count int
}

// FetchIrelandNewsHeadlines uses Gemini with Google Search grounding to retrieve
// today's Ireland daily-life news formatted for a Threads text post (<= 500 chars).
func (c *Client) FetchIrelandNewsHeadlines(ctx context.Context, language string, count int) (*NewsResult, error) {
	if c.apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY is not set")
	}
	if count <= 0 {
		count = 3
	}

	prompt, err := c.renderPrompt(language, count)
	if err != nil {
		return nil, err
	}

	reqPayload := generateRequest{
		Contents: []content{
			{
				Parts: []part{{Text: prompt}},
			},
		},
		Tools: []tool{
			{GoogleSearch: &googleSearchTool{}},
		},
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/%s:generateContent", baseURL, c.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call gemini api: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read gemini response: %w", err)
	}

	var parsed generateResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode gemini response (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("gemini api error (%d %s): %s", parsed.Error.Code, parsed.Error.Status, parsed.Error.Message)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gemini api returned status %d: %s", resp.StatusCode, string(respBody))
	}
	if len(parsed.Candidates) == 0 {
		return nil, errors.New("gemini returned no candidates")
	}

	var sb strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	rawText := strings.TrimSpace(sb.String())
	if rawText == "" {
		return nil, errors.New("gemini returned empty text")
	}

	postBody, promptURLs := splitBodyAndSources(rawText)
	if postBody == "" {
		return nil, errors.New("gemini returned empty post body")
	}
	text := truncateRunes(postBody, maxThreadsPostRunes)

	var sources []Source
	seen := make(map[string]struct{})
	// 1. Prefer verified Google Search grounding chunks first.
	for _, chunk := range parsed.Candidates[0].GroundingMetadata.GroundingChunks {
		if chunk.Web == nil || chunk.Web.URI == "" {
			continue
		}
		resolvedURI := c.resolveRedirectURL(ctx, chunk.Web.URI)
		if _, ok := seen[resolvedURI]; ok {
			continue
		}
		seen[resolvedURI] = struct{}{}
		sources = append(sources, Source{
			Title: chunk.Web.Title,
			URI:   resolvedURI,
		})
	}
	// 2. Supplement / fallback with URLs explicitly returned after ---SOURCES---.
	for _, rawURI := range promptURLs {
		resolvedURI := c.resolveRedirectURL(ctx, rawURI)
		if _, ok := seen[resolvedURI]; ok {
			continue
		}
		seen[resolvedURI] = struct{}{}
		host := ""
		if u, err := url.Parse(resolvedURI); err == nil {
			host = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		}
		sources = append(sources, Source{
			Title: host,
			URI:   resolvedURI,
		})
	}

	return &NewsResult{
		PostText: text,
		Sources:  sources,
	}, nil
}

// splitBodyAndSources separates the Threads post body from URLs listed after "---SOURCES---".
func splitBodyAndSources(raw string) (string, []string) {
	const sep = "---SOURCES---"
	idx := strings.Index(raw, sep)
	if idx < 0 {
		return strings.TrimSpace(raw), nil
	}

	body := strings.TrimSpace(raw[:idx])
	tail := raw[idx+len(sep):]

	var urls []string
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-・*• ")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Extract the first https:// or http:// token on the line.
		start := strings.Index(line, "https://")
		if start < 0 {
			start = strings.Index(line, "http://")
		}
		if start < 0 {
			continue
		}
		candidate := line[start:]
		if fields := strings.Fields(candidate); len(fields) > 0 {
			candidate = strings.TrimRight(fields[0], ".,)>]\"'")
		}
		if u, err := url.Parse(candidate); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
			urls = append(urls, candidate)
		}
	}
	return body, urls
}

// resolveRedirectURL resolves Gemini grounding redirect URLs to the actual article URL.
func (c *Client) resolveRedirectURL(ctx context.Context, rawURL string) string {
	if !strings.Contains(rawURL, "vertexaisearch.cloud.google.com/grounding-api-redirect/") {
		return rawURL
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(resolveCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return rawURL
	}
	resp, err := client.Do(req)
	if err != nil {
		return rawURL
	}
	defer resp.Body.Close()

	if loc := strings.TrimSpace(resp.Header.Get("Location")); loc != "" {
		return loc
	}
	return rawURL
}

func (c *Client) renderPrompt(language string, count int) (string, error) {
	lang := "ja"
	if strings.EqualFold(strings.TrimSpace(language), "en") {
		lang = "en"
	}

	filename := fmt.Sprintf("ireland_news_%s.txt", lang)
	path := filepath.Join(c.promptsDir, filename)

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read prompt file %s: %w", path, err)
	}

	tmpl, err := template.New(filename).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse prompt template %s: %w", path, err)
	}

	var buf bytes.Buffer
	data := promptData{
		Date:  time.Now().UTC().Format("2006-01-02"),
		Count: count,
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute prompt template %s: %w", path, err)
	}

	return strings.TrimSpace(buf.String()), nil
}

func truncateRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if maxRunes <= 1 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-1]) + "…"
}
