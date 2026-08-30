# Stage 1: build a fully static binary. CGO is disabled so the result runs on
# a scratch image with no libc; the SQLite driver is pure Go for this reason.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# Tests run inside the image build so a broken build can never be published.
RUN CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./...
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /server ./cmd/server
# Empty directory to seed the data volume with the right owner (see below).
RUN mkdir -p /data

# Stage 2: minimal runtime. Non-root user, no shell.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /server /app/server
# /app/data is where the SQLite file lives; compose mounts a named volume here.
# A named volume is initialised from the image's directory *including its
# owner*, so this directory must already belong to nonroot in the image —
# otherwise the process (uid 65532) cannot create the database file.
COPY --from=build --chown=nonroot:nonroot /data /app/data
VOLUME ["/app/data"]
ENV PORT=8080 DB_PATH=/app/data/chat.db
EXPOSE 8080
ENTRYPOINT ["/app/server"]
