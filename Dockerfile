# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /job ./cmd/job

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=builder /job /job
COPY prompts/ /prompts/

ENV PROMPTS_DIR=/prompts

USER nonroot:nonroot
ENTRYPOINT ["/job"]
