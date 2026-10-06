# Build (cgo is needed for the SQLite driver)
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/certsforever ./cmd/certsforever \
 && mkdir -p /out/data

# Run
FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/certsforever /certsforever
COPY --from=build --chown=65532:65532 /out/data /data
ENV CERTS_ADDR=:8080 \
    CERTS_DB=/data/certs.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/certsforever"]
CMD ["serve"]
