# syntax=docker/dockerfile:1
# agent-web-ui: the web server. Build from the repository root:
#   docker build -t agent-web-ui .

FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent-web-ui ./cmd/web

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/agent-web-ui /agent-web-ui
USER nonroot:nonroot
EXPOSE 8300
# Distroless has no curl; the binary checks itself: ["/agent-web-ui", "healthcheck"].
ENTRYPOINT ["/agent-web-ui"]
