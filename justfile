# List recipes.
default:
    @just --list

# Build the binary into ./acplause.
build:
    go build -o acplause ./cmd/acplause

# Install the binary into $GOBIN.
install:
    go install ./cmd/acplause

# Run all tests with the race detector. -count=1 because the e2e tests
# build the binary themselves, so go test cannot see when its sources change
# and would replay stale cached results.
test *args:
    go test -race -count=1 {{ args }} ./...

# Run only the end-to-end tests.
e2e *args:
    go test -race -count=1 {{ args }} ./e2e/

# Run the macOS-only ADR 0025 probes; unmet sandbox gates deliberately fail.
seatbelt-prototype *args:
    go test -race -count=1 -tags seatbelt_prototype -v {{ args }} ./experiments/seatbelt/

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

# Everything CI should run; optional arguments are passed to go test.
check *args: fmt-check lint (test args) release-check
    go mod tidy -diff

# Run the agent on stdio; the ACP client selects the model.
run:
    go run ./cmd/acplause

# Remove build output.
clean:
    rm -f acplause

# Dispatch the release workflow on main; all builds happen in Actions.
release:
    gh workflow run release.yml --ref main

# Exercise release retry/version/checksum invariants without publishing.
release-check:
    python3 -m unittest discover -s scripts -p '*_test.py'
