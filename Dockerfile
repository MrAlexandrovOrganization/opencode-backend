# check=skip=InvalidDefaultArgInFrom

# Required version arguments are supplied by Make/Compose, without fallback pins.
ARG GO_VERSION
ARG ALPINE_VERSION
FROM golang:${GO_VERSION}-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o opencode-backend ./cmd/backend

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/opencode-backend .
EXPOSE 8080
ENTRYPOINT ["./opencode-backend"]
