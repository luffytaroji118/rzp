FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/solver .

FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata \
    chromium nss freetype harfbuzz ttf-freefont \
    && addgroup -S solver && adduser -S solver -G solver

ENV CHROME_BIN=/usr/bin/chromium-browser

WORKDIR /app
COPY --from=builder /app/solver .

USER solver

EXPOSE 8080

CMD ["./solver"]
