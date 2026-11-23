package threads

import (
	"strings"
	"unicode/utf8"
)

// SourceLink represents a resolved reference URL returned from news search.
type SourceLink struct {
	Title string
	URI   string
}

// BuildSourceReplies formats resolved source URLs into one or more Threads replies,
// staying safely within Threads' 500-character limit and 5-unique-links-per-post limit.
func BuildSourceReplies(sources []SourceLink, language string) []string {
	if len(sources) == 0 {
		return nil
	}

	const (
		maxReplyRunes    = 480
		maxSources       = 4
		maxLinksPerReply = 4 // Threads allows at most 5 unique links per post.
	)

	header := "🔗 参考リンク（記事元）"
	if strings.EqualFold(language, "en") {
		header = "🔗 Source links"
	}

	var urls []string
	seenDomain := make(map[string]struct{})
	for _, s := range sources {
		if len(urls) >= maxSources {
			break
		}
		uri := strings.TrimSpace(s.URI)
		if uri == "" {
			continue
		}
		if strings.Contains(uri, "vertexaisearch.cloud.google.com/grounding-api-redirect/") && utf8.RuneCountInString(uri) > 150 {
			continue
		}
		domain := strings.ToLower(strings.TrimSpace(s.Title))
		if domain != "" {
			if _, exists := seenDomain[domain]; exists {
				continue
			}
			seenDomain[domain] = struct{}{}
		}
		entry := "・" + uri
		if utf8.RuneCountInString(entry) > maxReplyRunes-30 {
			continue
		}
		urls = append(urls, entry)
	}

	if len(urls) == 0 {
		return nil
	}

	var replies []string
	current := header
	linksInCurrent := 0
	for _, entry := range urls {
		candidate := current + "\n" + entry
		if linksInCurrent < maxLinksPerReply && utf8.RuneCountInString(candidate) <= maxReplyRunes {
			current = candidate
			linksInCurrent++
			continue
		}
		if current != header {
			replies = append(replies, current)
		}
		current = header + "\n" + entry
		linksInCurrent = 1
	}
	if current != header {
		replies = append(replies, current)
	}
	return replies
}
