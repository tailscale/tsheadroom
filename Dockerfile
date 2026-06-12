# Stage 1: build the Go binary
FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /build/tsheadroom .

# Stage 2: Python runtime with headroom-ai
FROM python:3.13-slim
ARG HEADROOM_VARIANT=base

RUN python -m venv /venv

# Install the chosen headroom-ai variant.
# python:3.13-slim ships prebuilt wheels so no Rust toolchain is needed.
# 'ml' adds Kompress (~600 MB ML model downloaded on first use, cached in a volume).
RUN if [ "$HEADROOM_VARIANT" = "ml" ]; then \
      /venv/bin/pip install --no-cache-dir 'headroom-ai[ml]'; \
    else \
      /venv/bin/pip install --no-cache-dir 'headroom-ai'; \
    fi

COPY --from=builder /build/tsheadroom /app/tsheadroom
COPY worker.py /app/worker.py

ENTRYPOINT ["/app/tsheadroom"]
