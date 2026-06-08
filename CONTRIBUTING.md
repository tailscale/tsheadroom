# Contributing to tsheadroom

Thanks for your interest in contributing!

## Developer Certificate of Origin

We require a [Developer Certificate of Origin](https://developercertificate.org/)
(DCO) `Signed-off-by` line on every commit. Sign off with `git commit -s`, which
appends:

```
Signed-off-by: Your Name <you@example.com>
```

This certifies that you wrote the change or otherwise have the right to submit it
under the project's license.

## License headers

Every Go and Python source file must begin with the Tailscale copyright and
SPDX header:

```go
// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause
```

Use `#` comments for Python (after the shebang, if any). The `TestLicenseHeaders`
test enforces this, so CI fails if a file is missing it.

## Building and testing

```bash
make            # build ./build/tsheadroom
make test       # Go tests (incl. license headers) + Python worker tests
```

The Python tests run against a fake `headroom` by default. To also run the
real-headroom integration test, point `PYTHON` at an interpreter that has
`headroom-ai` installed:

```bash
make test PYTHON=/path/to/venv/bin/python
```

Please keep `gofmt` and `go vet ./...` clean. CI runs the build, `go vet`, the Go
test suite (which includes the license-header check), and the Python tests.

## Reporting security issues

Please do not open public issues for security vulnerabilities. See
[SECURITY.md](SECURITY.md) for private reporting.
