###############################################################
# BUILDER
###############################################################
FROM golang:1.26-alpine AS builder

ENV CGO_ENABLED=0

# Build identity, surfaced by GET /v1/version. Pass via --build-arg.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -trimpath -ldflags="-s -w \
	-X github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/version.Version=${VERSION} \
	-X github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/version.Commit=${COMMIT} \
	-X github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/version.Date=${DATE}" \
	-o /out/guardrails-llm-filter-extproc ./cmd/guardrails-llm-filter-extproc

###############################################################
# RUNTIME
###############################################################
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/guardrails-llm-filter-extproc /app/guardrails-llm-filter-extproc
COPY --from=builder /src/configs /app/configs

# ext_proc gRPC, health gRPC, metrics HTTP, configuration API HTTP
EXPOSE 9000 9005 9080 9090 9091

ENTRYPOINT ["/app/guardrails-llm-filter-extproc"]
