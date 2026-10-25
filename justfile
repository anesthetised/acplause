# Use the OS-provided shell on Windows; Git Bash is not required.
set windows-shell := ["powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command"]

binary := if os() == "windows" { "acplause.exe" } else { "acplause" }

# List recipes.
default:
    @just --list

# Build the native binary into the current directory.
build:
    go build -o {{ binary }} ./cmd/acplause

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
[unix]
fmt-check:
    @test -z "$(gofmt -l .)" || (gofmt -l . && echo "run: just fmt" && exit 1)

[windows]
fmt-check:
    $files = gofmt -l .; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; if ($files) { $files; Write-Error 'run: just fmt'; exit 1 }

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
[unix]
clean:
    rm -f {{ binary }}

[windows]
clean:
    if (Test-Path {{ binary }}) { Remove-Item {{ binary }} }

# Dispatch the release workflow on main; all builds happen in Actions.
release:
    gh workflow run release.yml --ref main

# Exercise release retry/version/checksum invariants without publishing.
[unix]
release-check:
    python3 -m unittest discover -s scripts -p '*_test.py'

[windows]
release-check:
    python -m unittest discover -s scripts -p '*_test.py'
