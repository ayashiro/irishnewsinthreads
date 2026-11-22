# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /server ./cmd/server

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=builder /server /server
COPY prompts/ /prompts/

ENV PORT=8080
ENV PROMPTS_DIR=/prompts
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/server"]
