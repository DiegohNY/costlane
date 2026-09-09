FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# CGO is off so the binary runs on a distroless static base.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/costlane ./cmd/costlane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/costlane /costlane
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/costlane"]
