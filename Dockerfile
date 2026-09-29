FROM golang:1.27.1-alpine3.24 AS deps
WORKDIR /src
ARG GOPROXY=https://goproxy.cn
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download

FROM deps AS adapter-build
COPY cmd/adapter ./cmd/adapter
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o /adapter ./cmd/adapter

FROM deps AS probe-build
COPY cmd/teambition-probe ./cmd/teambition-probe
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o /teambition-probe ./cmd/teambition-probe

# Static Go binaries still need trusted roots for outbound HTTPS.
FROM scratch AS teambition-probe
COPY --from=deps /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=probe-build /teambition-probe /teambition-probe
USER 65532:65532
ENTRYPOINT ["/teambition-probe"]

FROM scratch AS adapter
COPY --from=deps /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=adapter-build /adapter /adapter
EXPOSE 8787
USER 65532:65532
ENTRYPOINT ["/adapter"]
