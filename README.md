# Threads Ireland News Bot (Go + Gemini + Cloud Run)

A Go-based Google Cloud Run backend that fetches daily-life news from Ireland using the Gemini API (with Google Search grounding) and publishes concise posts to Threads via the official Meta Threads Graph API.

Built with **zero external dependencies** (Go standard library only), making it fast and reliable to build and deploy directly to Cloud Run.

## Project Structure

```text
.
├── cmd/server/main.go          # HTTP server for Cloud Run
├── internal/
│   ├── config/config.go        # Environment variable configuration
│   ├── gemini/client.go        # Gemini REST API + Google Search grounding
│   └── threads/client.go       # Meta Threads Graph API (2-step publish & token refresh)
├── prompts/
│   ├── ireland_news_ja.txt     # Japanese prompt template (daily life focus)
│   └── ireland_news_en.txt     # English prompt template (daily life focus)
├── Dockerfile                  # Multi-stage build (golang:1.23-alpine -> distroless)
├── go.mod
├── .env.example
└── .gitignore
```

## Customizing Prompts

Gemini prompts are stored outside the Go source code under `prompts/`. At runtime, `{{.Date}}` (UTC `YYYY-MM-DD`) and `{{.Count}}` are automatically injected:

- **Japanese**: `prompts/ireland_news_ja.txt` — tailored for Japanese residents, working holiday makers, language students, and expats in Ireland (weather/Met Éireann alerts, cost of living, housing, visa/IRP, public transport, and local events).
- **English**: `prompts/ireland_news_en.txt`

You can adjust tone, topics, or hashtags simply by editing these `.txt` files without touching the Go code.

## API Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health check |
| `POST` | `/preview` | Generate a post via Gemini without publishing to Threads |
| `POST` | `/publish` | Generate via Gemini and publish to Threads (supports `{"dry_run": true}`) |
| `GET` | `/threads/me` | Verify the configured Threads token and return `id` & `username` |
| `POST` | `/threads/refresh-token` | Refresh a long-lived Threads access token (60-day validity) |

> If `API_SECRET` is set, all endpoints except `/health` require `X-API-Secret: <API_SECRET>` or `Authorization: Bearer <API_SECRET>`.

---

## Meta Threads API Setup

To publish text posts via the official Threads API, you need **`THREADS_USER_ID`** and a long-lived **`THREADS_ACCESS_TOKEN`**.

### 1. Create a Meta App
1. Go to [Meta for Developers](https://developers.facebook.com/) → **My Apps** → **Create App**.
2. Choose the **Access the Threads API** use case.
3. Under **Use cases → Customize**, add the following permissions:
   - `threads_basic`
   - `threads_content_publish`

### 2. Add and Approve a Threads Tester
1. Go to **App roles → Roles → Add People**, select **Threads Tester**, and add your Threads account.
2. In the Threads app or web (`threads.net`), open **Settings → Account → Website permissions → Invitations** and accept the invitation.
   *(Tip: Make sure your Threads account is set to **Public** while testing, and check the User Token Generator directly even if the Roles page briefly still shows "Pending".)*

### 3. Generate a Long-Lived Access Token
- **Option A (Recommended for personal bots): User Token Generator**
  1. Go to **Use cases → Customize → Settings → User Token Generator**.
  2. Click **Generate Token** next to your approved Threads account.
- **Option B: Standard OAuth 2.0 Flow**
  1. Authorize with `scope=threads_basic,threads_content_publish`.
  2. Exchange the authorization code at `POST https://graph.threads.net/oauth/access_token` for a 1-hour short-lived token.
  3. Exchange for a 60-day long-lived token at:
     `GET https://graph.threads.net/access_token?grant_type=th_exchange_token&client_secret=<APP_SECRET>&access_token=<SHORT_LIVED_TOKEN>`

### 4. Retrieve `THREADS_USER_ID`
```bash
curl "https://graph.threads.net/v1.0/me?fields=id,username&access_token=<THREADS_ACCESS_TOKEN>"
```
Use the returned `"id"` as `THREADS_USER_ID`.

### 5. Token Refresh (Every 60 Days)
Long-lived tokens expire after 60 days. Once a token is at least 24 hours old (and before it expires), refresh it via:
```bash
curl -G "https://graph.threads.net/refresh_access_token" \
  -d "grant_type=th_refresh_token" \
  -d "access_token=<THREADS_ACCESS_TOKEN>"
```

### How Publishing Works
`internal/threads/client.go` implements Meta's two-step container workflow:
1. `POST https://graph.threads.net/v1.0/{user_id}/threads` (`media_type=TEXT`, `text=...`) → returns `creation_id`.
2. Wait briefly (3 seconds), then `POST https://graph.threads.net/v1.0/{user_id}/threads_publish` (`creation_id=...`) → publishes the post.

---

## Deploying to Google Cloud Run

From WSL, navigate to the project directory and deploy with `gcloud`:

```bash
cd "/home/kenta/threads news"

gcloud run deploy threads-ireland-news \
  --source . \
  --region asia-northeast1 \
  --no-allow-unauthenticated \
  --set-env-vars "GEMINI_API_KEY=YOUR_GEMINI_KEY,THREADS_USER_ID=YOUR_USER_ID,THREADS_ACCESS_TOKEN=YOUR_TOKEN,POST_LANGUAGE=ja,NEWS_COUNT=3,API_SECRET=YOUR_SECRET"
```

> For production, store secrets in **Google Cloud Secret Manager** and inject them with `--set-secrets`.

### Test Preview (Dry Run)
```bash
curl -X POST "https://<YOUR_CLOUD_RUN_URL>/preview" \
  -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
  -H "X-API-Secret: YOUR_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"language":"ja","count":3}'
```

### Scheduled Posting
Create a **Cloud Scheduler** job to send an authenticated `POST` request to `https://<YOUR_CLOUD_RUN_URL>/publish` on your desired schedule (e.g., daily at 08:00 Europe/Dublin).
