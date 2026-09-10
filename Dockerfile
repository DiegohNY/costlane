FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# VERSION is stamped into the binary and reported by /healthz. It defaults to
# "dev" so a local build says what it is rather than claiming a release.
ARG VERSION=dev
# CGO is off so the binary runs on a distroless static base.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/costlane ./cmd/costlane
# The fake provider ships in the same image so that docker compose needs one
# build, and so the quickstart never asks for a provider credential.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/fakeprovider ./cmd/fakeprovider

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/costlane /costlane
COPY --from=build /out/fakeprovider /fakeprovider
USER nonroot:nonroot
EXPOSE 8080 8081 9090
ENTRYPOINT ["/costlane"]
