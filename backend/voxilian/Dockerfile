# Voxilian backend image (plan M0-T3). Single binary serves all
# subcommands: serve / migrate / admin / seed (spec §10).
# Build from backend/voxilian as context:
#   docker build -f backend/voxilian/Dockerfile backend/voxilian

ARG GO_VERSION=1.27

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
# Dependencies first for layer caching.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG REVISION=none
RUN CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w -X github.com/dlukt/voxilian/internal/observe.Version=${VERSION} -X github.com/dlukt/voxilian/internal/observe.Revision=${REVISION}" \
	-o /out/voxilian .

FROM alpine:3.22
# CA certificates for future HTTPS/JWKS calls (spec D6/§11).
RUN apk add --no-cache ca-certificates && \
	adduser -D -H -u 65532 voxilian
USER 65532:65532
COPY --from=build /out/voxilian /usr/local/bin/voxilian
ENTRYPOINT ["/usr/local/bin/voxilian"]
