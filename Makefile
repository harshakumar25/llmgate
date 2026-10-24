.PHONY: run build test eval vet docker up down tidy

run:          ## Run the gateway (uses config.yaml if present)
	LLMGATE_CONFIG=$${LLMGATE_CONFIG:-config.yaml} go run ./cmd/llmgate

build:        ## Compile the binary to ./bin/llmgate
	go build -o bin/llmgate ./cmd/llmgate

test:         ## Run all unit tests
	go test ./... -count=1

eval:         ## Run the golden-set eval against a live gateway on :8080
	go run ./cmd/eval -url http://localhost:8080/v1/chat

vet:          ## Static checks
	go vet ./...

docker:       ## Build the Docker image
	docker build -t llmgate:dev .

up:           ## Start gateway + Postgres + Redis
	docker compose up --build

down:         ## Stop the stack
	docker compose down

tidy:         ## Tidy modules
	go mod tidy
