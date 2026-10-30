ARG GO_VERSION
FROM golang:${GO_VERSION}-alpine AS builder
ARG VERSION=dev
ARG REVISION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.Gitsha=${REVISION}" \
    -trimpath -o /out/external-dns-netbird-webhook ./cmd/external-dns-netbird-webhook

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/external-dns-netbird-webhook /external-dns-netbird-webhook
EXPOSE 8888/tcp
ENTRYPOINT ["/external-dns-netbird-webhook"]
