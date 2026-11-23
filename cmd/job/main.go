package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"threads-news/internal/config"
	"threads-news/internal/gemini"
	"threads-news/internal/threads"
)

type jobOutput struct {
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

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	logger.Info("starting Ireland news Threads job", "model", cfg.GeminiModel, "language", cfg.PostLanguage, "dry_run", cfg.DryRun)

	geminiClient := gemini.NewClient(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.PromptsDir)
	news, err := geminiClient.FetchIrelandNewsHeadlines(ctx, cfg.PostLanguage, cfg.NewsCount)
	if err != nil {
		logger.Error("failed to fetch news from gemini", "error", err)
		os.Exit(1)
	}

	links := make([]threads.SourceLink, 0, len(news.Sources))
	for _, s := range news.Sources {
		links = append(links, threads.SourceLink{Title: s.Title, URI: s.URI})
	}
	replyTexts := threads.BuildSourceReplies(links, cfg.PostLanguage)

	out := jobOutput{
		DryRun:     cfg.DryRun,
		PostText:   news.PostText,
		ReplyTexts: replyTexts,
		Sources:    news.Sources,
	}

	if cfg.DryRun {
		printJSON(out)
		logger.Info("dry run completed; skipped publishing to Threads")
		return
	}

	threadsClient := threads.NewClient(cfg.ThreadsUserID, cfg.ThreadsAccessToken)
	pubRes, err := threadsClient.PublishTextPost(ctx, news.PostText)
	if err != nil {
		logger.Error("failed to publish to threads", "error", err)
		os.Exit(1)
	}
	out.Threads = pubRes
	logger.Info("published Ireland news to Threads", "post_id", pubRes.PostID, "container_id", pubRes.ContainerID)

	parentID := pubRes.PostID
	for i, replyText := range replyTexts {
		var repRes *threads.PublishResult
		var repErr error
		for attempt := 1; attempt <= 3; attempt++ {
			select {
			case <-ctx.Done():
				logger.Error("context cancelled while waiting to publish reply", "error", ctx.Err())
				os.Exit(1)
			case <-time.After(6 * time.Second):
			}

			repRes, repErr = threadsClient.PublishReplyPost(ctx, parentID, replyText)
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
		out.Replies = append(out.Replies, *repRes)
		parentID = repRes.PostID
	}

	printJSON(out)
	logger.Info("job completed successfully")
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
