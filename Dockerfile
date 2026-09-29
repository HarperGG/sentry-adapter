# syntax=docker/dockerfile:1
FROM golang:1.27.1-bookworm AS deps
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

FROM deps AS adapter-build
COPY cmd/adapter ./cmd/adapter
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o /adapter ./cmd/adapter

FROM deps AS probe-build
COPY cmd/teambition-probe ./cmd/teambition-probe
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o /teambition-probe ./cmd/teambition-probe

FROM gcr.io/distroless/static-debian13:nonroot AS teambition-probe
COPY --from=probe-build /teambition-probe /teambition-probe
USER nonroot:nonroot
ENTRYPOINT ["/teambition-probe"]

FROM gcr.io/distroless/static-debian13:nonroot AS adapter
COPY --from=adapter-build /adapter /adapter
EXPOSE 8787
USER nonroot:nonroot
ENTRYPOINT ["/adapter"]
