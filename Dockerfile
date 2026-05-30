# --- build stage ---
FROM golang:1.24-alpine AS build
WORKDIR /src

# No external deps, but keep the layer cache friendly.
COPY go.mod ./
COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /ru-list-server .

# --- runtime stage ---
FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source="https://github.com/alexeynavarkin/mikrotik-list-gen"
LABEL org.opencontainers.image.description="RU IP address-list generator for MikroTik (.rsc over HTTP)"
LABEL org.opencontainers.image.licenses="MIT"
COPY --from=build /ru-list-server /ru-list-server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/ru-list-server"]
