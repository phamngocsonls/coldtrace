# ColdTrace gateway — build stage pins to current stable Go toolchain
FROM golang:1.27.3-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/gateway ./cmd/gateway

# Runtime: distroless nonroot (no shell, minimal surface)
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/gateway /gateway
EXPOSE 8080
ENTRYPOINT ["/gateway"]
