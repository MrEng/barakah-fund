# Multi-stage build for Cloud Run. Produces a small static binary.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
# No external dependencies: nothing to download.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
# Cloud Run sets $PORT; the server reads it (default 8080).
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/api"]
