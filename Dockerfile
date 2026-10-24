# --- build stage ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
# No external deps yet; this still primes the module cache once you add them.
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/llmgate ./cmd/llmgate

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/llmgate /llmgate
COPY config.example.yaml /config.yaml
ENV LLMGATE_CONFIG=/config.yaml
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/llmgate"]
