package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	DryRun   bool                   `json:"dry_run"`
	PostText string                 `json:"post_text"`
	Sources  []gemini.Source        `json:"sources,omitempty"`
	Threads  *threads.PublishResult `json:"threads,omitempty"`
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

		writeJSON(w, http.StatusOK, publishResponse{
			DryRun:   true,
			PostText: news.PostText,
			Sources:  news.Sources,
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

		if dryRun {
			writeJSON(w, http.StatusOK, publishResponse{
				DryRun:   true,
				PostText: news.PostText,
				Sources:  news.Sources,
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
		writeJSON(w, http.StatusOK, publishResponse{
			DryRun:   false,
			PostText: news.PostText,
			Sources:  news.Sources,
			Threads:  pubRes,
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
