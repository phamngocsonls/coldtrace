# ColdTrace ops-api — build stage pins to current stable Go toolchain
FROM golang:1.27.3-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ops-api ./cmd/ops-api

# Runtime: distroless nonroot
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/ops-api /ops-api
EXPOSE 8092
ENTRYPOINT ["/ops-api"]
