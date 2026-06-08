# tsheadroom — aperture pre_request guardrail that compresses LLM requests
# with Headroom over tsnet.
#
# Common targets:
#   make            production build -> build/tsheadroom
#   make test       run Go + Python tests
#   make test-go    Go tests only
#   make test-py    Python (worker.py) tests only
#
# The Python tests need an interpreter for the worker. Override PYTHON to point
# at one that has headroom-ai installed; doing so also enables the real-headroom
# integration test (it is skipped otherwise):
#
#   make test PYTHON=/path/to/venv/bin/python
#   export PYTHON=/path/to/venv/bin/python && make test

GO     ?= go
PYTHON ?= python3
BIN    ?= build/tsheadroom

.PHONY: all build test test-go test-py fmt vet clean

all: build

build:
	$(GO) build -o $(BIN) .

test: test-go test-py

test-go:
	$(GO) test ./...

# Test modules self-insert the repo root for `import worker`; the suite
# self-skips the integration test when $(PYTHON) lacks headroom-ai.
test-py:
	$(PYTHON) -m unittest discover -s tests -p 'test_*.py' -v

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf build
