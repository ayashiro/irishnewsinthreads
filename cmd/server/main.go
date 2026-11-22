package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"threads-news/internal/config"
	"threads-news/internal/gemini"
	"threads-news/internal/threads"
)

type publishRequest struct {
	DryRun   *bool  `json:"dry_run,omitempty"`
	Language string `json:"language,omitempty"`
	Count    int    `json:"count,omitempty"`
}

type publishResponse struct {
	DryRun     bool                    `json:"dry_run"`
	PostText   string                  `json:"post_text"`
	ReplyTexts []string                `json:"reply_texts,omitempty"`
	Sources    []gemini.Source         `json:"sources,omitempty"`
	Threads    *threads.PublishResult  `json:"threads,omitempty"`
	Replies    []threads.PublishResult `json:"replies,omitempty"`
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config.Load()

	geminiClient := gemini.NewClient(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.PromptsDir)
	threadsClient := threads.NewClient(cfg.ThreadsUserID, cfg.ThreadsAccessToken)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// POST /preview generates the Ireland news post with Gemini without publishing to Threads.
	mux.HandleFunc("POST /preview", withAuth(cfg.APISecret, func(w http.ResponseWriter, r *http.Request) {
		reqBody, err := decodePublishRequest(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		lang := firstNonEmpty(reqBody.Language, cfg.PostLanguage)
		count := reqBody.Count
		if count <= 0 {
			count = cfg.NewsCount
		}

		news, err := geminiClient.FetchIrelandNewsHeadlines(r.Context(), lang, count)
		if err != nil {
			logger.Error("failed to fetch news from gemini", "error", err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}

		replyTexts := buildSourceReplies(news.Sources, lang)
		writeJSON(w, http.StatusOK, publishResponse{
			DryRun:     true,
			PostText:   news.PostText,
			ReplyTexts: replyTexts,
			Sources:    news.Sources,
		})
	}))

	// POST /publish fetches Ireland news via Gemini and publishes to Threads (unless dry_run=true).
	mux.HandleFunc("POST /publish", withAuth(cfg.APISecret, func(w http.ResponseWriter, r *http.Request) {
		reqBody, err := decodePublishRequest(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		dryRun := cfg.DryRun
		if reqBody.DryRun != nil {
			dryRun = *reqBody.DryRun
		}
		lang := firstNonEmpty(reqBody.Language, cfg.PostLanguage)
		count := reqBody.Count
		if count <= 0 {
			count = cfg.NewsCount
		}

		news, err := geminiClient.FetchIrelandNewsHeadlines(r.Context(), lang, count)
		if err != nil {
			logger.Error("failed to fetch news from gemini", "error", err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}

		replyTexts := buildSourceReplies(news.Sources, lang)

		if dryRun {
			writeJSON(w, http.StatusOK, publishResponse{
				DryRun:     true,
				PostText:   news.PostText,
				ReplyTexts: replyTexts,
				Sources:    news.Sources,
			})
			return
		}

		pubRes, err := threadsClient.PublishTextPost(r.Context(), news.PostText)
		if err != nil {
			logger.Error("failed to publish to threads", "error", err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}

		logger.Info("published Ireland news to Threads", "post_id", pubRes.PostID, "container_id", pubRes.ContainerID)

		var replies []threads.PublishResult
		parentID := pubRes.PostID
		for i, replyText := range replyTexts {
			var repRes *threads.PublishResult
			var repErr error
			// Threads takes a few seconds to index a newly published post before it accepts reply_to_id.
			for attempt := 1; attempt <= 3; attempt++ {
				select {
				case <-r.Context().Done():
					writeError(w, http.StatusRequestTimeout, r.Context().Err().Error())
					return
				case <-time.After(6 * time.Second):
				}

				repRes, repErr = threadsClient.PublishReplyPost(r.Context(), parentID, replyText)
				if repErr == nil {
					break
				}
				logger.Warn("reply attempt failed; retrying", "reply_index", i, "attempt", attempt, "parent_id", parentID, "error", repErr)
			}
			if repErr != nil {
				logger.Error("failed to publish reply to threads", "reply_index", i, "parent_id", parentID, "error", repErr)
				break
			}
			logger.Info("published source links reply to Threads", "reply_index", i, "post_id", repRes.PostID, "parent_id", parentID)
			replies = append(replies, *repRes)
			parentID = repRes.PostID
		}

		writeJSON(w, http.StatusOK, publishResponse{
			DryRun:     false,
			PostText:   news.PostText,
			ReplyTexts: replyTexts,
			Sources:    news.Sources,
			Threads:    pubRes,
			Replies:    replies,
		})
	}))

	// GET /threads/me verifies the configured Threads access token and returns user_id / username.
	mux.HandleFunc("GET /threads/me", withAuth(cfg.APISecret, func(w http.ResponseWriter, r *http.Request) {
		profile, err := threadsClient.GetMe(r.Context())
		if err != nil {
			logger.Error("failed to fetch threads profile", "error", err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, profile)
	}))

	// POST /threads/refresh-token refreshes the long-lived Threads token (valid for 60 days).
	mux.HandleFunc("POST /threads/refresh-token", withAuth(cfg.APISecret, func(w http.ResponseWriter, r *http.Request) {
		res, err := threadsClient.RefreshLongLivedToken(r.Context())
		if err != nil {
			logger.Error("failed to refresh threads token", "error", err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("starting server", "port", cfg.Port, "model", cfg.GeminiModel)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down server")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func withAuth(secret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			next(w, r)
			return
		}
		provided := strings.TrimSpace(r.Header.Get("X-API-Secret"))
		if provided == "" {
			authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
			if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
				provided = strings.TrimSpace(authHeader[7:])
			}
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func decodePublishRequest(r *http.Request) (publishRequest, error) {
	var req publishRequest
	if r.Body == nil {
		return req, nil
	}
	defer r.Body.Close()

	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return req, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return req, nil
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return req, err
	}
	return req, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// buildSourceReplies formats resolved source URLs into one or more Threads replies,
// each safely within Threads' 500-character limit.
func buildSourceReplies(sources []gemini.Source, language string) []string {
	if len(sources) == 0 {
		return nil
	}

	const maxReplyRunes = 480
	const maxSources = 4

	header := "🔗 参考リンク（記事元）"
	if strings.EqualFold(language, "en") {
		header = "🔗 Source links"
	}

	var entries []string
	for _, s := range sources {
		if len(entries) >= maxSources {
			break
		}
		uri := strings.TrimSpace(s.URI)
		if uri == "" {
			continue
		}
		// Skip unresolved Vertex redirect URLs that are too long and ugly for users.
		if strings.Contains(uri, "vertexaisearch.cloud.google.com/grounding-api-redirect/") && utf8.RuneCountInString(uri) > 150 {
			continue
		}
		title := strings.TrimSpace(s.Title)
		var entry string
		if title != "" {
			entry = fmt.Sprintf("・%s\n%s", title, uri)
		} else {
			entry = fmt.Sprintf("・%s", uri)
		}
		if utf8.RuneCountInString(entry) > maxReplyRunes-30 {
			continue
		}
		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		return nil
	}

	var replies []string
	current := header
	for _, entry := range entries {
		candidate := current + "\n" + entry
		if utf8.RuneCountInString(candidate) <= maxReplyRunes {
			current = candidate
			continue
		}
		if current != header {
			replies = append(replies, current)
		}
		current = header + "\n" + entry
	}
	if current != header {
		replies = append(replies, current)
	}
	return replies
}
