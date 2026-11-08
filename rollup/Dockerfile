# ColdTrace downsampling service — build stage pins to current stable Go toolchain
FROM golang:1.27.3-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/rollup ./cmd/rollup

# Runtime: distroless nonroot
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/rollup /rollup
ENTRYPOINT ["/rollup"]
