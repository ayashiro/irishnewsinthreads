package threads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const graphBaseURL = "https://graph.threads.net"

// Client interacts with the official Meta Threads Graph API.
type Client struct {
	userID      string
	accessToken string
	httpClient  *http.Client
}

// NewClient creates a new Threads API client.
func NewClient(userID, accessToken string) *Client {
	return &Client{
		userID:      userID,
		accessToken: accessToken,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// PublishResult holds IDs returned by the 2-step Threads publish workflow.
type PublishResult struct {
	ContainerID string `json:"container_id"`
	PostID      string `json:"post_id"`
}

// Profile represents basic Threads user info from /me.
type Profile struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// TokenRefreshResult represents the response from refreshing a long-lived Threads token.
type TokenRefreshResult struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

type apiErrorResponse struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// PublishTextPost performs the 2-step Meta Threads API publishing flow:
// 1. Create a TEXT media container (POST /v1.0/{user_id}/threads)
// 2. Wait briefly, then publish it (POST /v1.0/{user_id}/threads_publish)
func (c *Client) PublishTextPost(ctx context.Context, text string) (*PublishResult, error) {
	if c.userID == "" {
		return nil, errors.New("THREADS_USER_ID is not set")
	}
	if c.accessToken == "" {
		return nil, errors.New("THREADS_ACCESS_TOKEN is not set")
	}
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("post text cannot be empty")
	}

	containerID, err := c.createTextContainer(ctx, text)
	if err != nil {
		return nil, err
	}

	// Meta recommends waiting a few seconds after container creation before publishing.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(3 * time.Second):
	}

	postID, err := c.publishContainer(ctx, containerID)
	if err != nil {
		return nil, err
	}

	return &PublishResult{
		ContainerID: containerID,
		PostID:      postID,
	}, nil
}

func (c *Client) createTextContainer(ctx context.Context, text string) (string, error) {
	endpoint := fmt.Sprintf("%s/v1.0/%s/threads", graphBaseURL, url.PathEscape(c.userID))
	form := url.Values{}
	form.Set("media_type", "TEXT")
	form.Set("text", text)
	form.Set("access_token", c.accessToken)

	body, err := c.doFormPost(ctx, endpoint, form)
	if err != nil {
		return "", fmt.Errorf("create threads container: %w", err)
	}

	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("decode container response: %w", err)
	}
	if res.ID == "" {
		return "", fmt.Errorf("missing container id in response: %s", string(body))
	}
	return res.ID, nil
}

func (c *Client) publishContainer(ctx context.Context, creationID string) (string, error) {
	endpoint := fmt.Sprintf("%s/v1.0/%s/threads_publish", graphBaseURL, url.PathEscape(c.userID))
	form := url.Values{}
	form.Set("creation_id", creationID)
	form.Set("access_token", c.accessToken)

	body, err := c.doFormPost(ctx, endpoint, form)
	if err != nil {
		return "", fmt.Errorf("publish threads container: %w", err)
	}

	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("decode publish response: %w", err)
	}
	if res.ID == "" {
		return "", fmt.Errorf("missing post id in response: %s", string(body))
	}
	return res.ID, nil
}

// GetMe fetches the authenticated Threads user's ID and username.
func (c *Client) GetMe(ctx context.Context) (*Profile, error) {
	if c.accessToken == "" {
		return nil, errors.New("THREADS_ACCESS_TOKEN is not set")
	}

	params := url.Values{}
	params.Set("fields", "id,username")
	params.Set("access_token", c.accessToken)
	endpoint := fmt.Sprintf("%s/v1.0/me?%s", graphBaseURL, params.Encode())

	body, err := c.doGet(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("get threads profile: %w", err)
	}

	var profile Profile
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, fmt.Errorf("decode profile response: %w", err)
	}
	return &profile, nil
}

// RefreshLongLivedToken refreshes a valid long-lived Threads access token (valid for 60 days).
// The token must be at least 24 hours old and not yet expired.
func (c *Client) RefreshLongLivedToken(ctx context.Context) (*TokenRefreshResult, error) {
	if c.accessToken == "" {
		return nil, errors.New("THREADS_ACCESS_TOKEN is not set")
	}

	params := url.Values{}
	params.Set("grant_type", "th_refresh_token")
	params.Set("access_token", c.accessToken)
	endpoint := fmt.Sprintf("%s/refresh_access_token?%s", graphBaseURL, params.Encode())

	body, err := c.doGet(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("refresh threads token: %w", err)
	}

	var result TokenRefreshResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode token refresh response: %w", err)
	}
	return &result, nil
}

func (c *Client) doFormPost(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.execute(req)
}

func (c *Client) doGet(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return c.execute(req)
}

func (c *Client) execute(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiErrorResponse
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != nil {
			return nil, fmt.Errorf("threads api error (%d %s): %s", apiErr.Error.Code, apiErr.Error.Type, apiErr.Error.Message)
		}
		return nil, fmt.Errorf("threads api returned status %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}
