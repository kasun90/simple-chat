.PHONY: run test vet build check docker-up docker-down demo

run:            ## Run locally on :8080 with seeded demo users
	SEED_USERS=alice,bob go run ./cmd/server

test:           ## Unit tests with the race detector
	go test -race -count=1 ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -o bin/server ./cmd/server

check: vet test build   ## The same gate CI and the Dockerfile enforce

docker-up:      ## Build the image (runs tests inside), start it, wait until healthy
	docker compose up --build -d --wait --wait-timeout 90

docker-down:
	docker compose down

demo: docker-up  ## Start the stack and open two browser windows (macOS `open`; falls back to printing URLs)
	@echo "Log in as alice in one window and bob in the other."
	@open -na "Google Chrome" --args --new-window http://localhost:8080 2>/dev/null || echo "  window 1: http://localhost:8080"
	@open -na "Google Chrome" --args --incognito http://localhost:8080 2>/dev/null || echo "  window 2: http://localhost:8080 (incognito)"
