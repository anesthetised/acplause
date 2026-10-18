# List recipes.
default:
    @just --list

# Build the binary into ./acplause.
build:
    go build -o acplause ./cmd/acplause

# Install the binary into $GOBIN.
install:
    go install ./cmd/acplause

# Run all tests with the race detector.
test *args:
    go test -race {{ args }} ./...

# Run only the end-to-end tests.
e2e *args:
    go test -race {{ args }} ./e2e/

# Format the code.
fmt:
    gofmt -w .

# Fail if any file is not gofmt-formatted.
fmt-check:
    @test -z "$(gofmt -l .)" || (gofmt -l . && echo "run: just fmt" && exit 1)

# Run go vet and staticcheck.
lint:
    go vet ./...
    staticcheck ./...

# Everything CI should run.
check: fmt-check lint test
    go mod tidy -diff

# Run the agent on stdio; the ACP client selects the model.
run:
    go run ./cmd/acplause

# Remove build output.
clean:
    rm -f acplause
