# Self-check, then hand off to a tiny runtime image.
#
# CGO is disabled so the binary is static: the runtime stage does not need glibc, and
# cross-compiling from a workstation keeps working.
#
# The Go version must match go.mod, which requires 1.26: os.Root's write methods
# (MkdirAll, Rename, RemoveAll) only exist from 1.25 onward and are what gives the
# tree layout its syscall-level path containment. Building with an older toolchain
# fails, and pinning it too low here is a build break that only shows up on deploy.
FROM golang:1.26-bookworm AS build
WORKDIR /src

# Cache the module graph separately from the source so edits do not re-download it.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Run the tests inside the build: a failing test should stop the image from being
# produced, not sit in a report nobody reads before the deploy.
RUN CGO_ENABLED=0 go vet ./... && \
    CGO_ENABLED=0 go test ./... -count=1 -timeout 300s

ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/newcloud ./cmd/newcloud

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/newcloud /newcloud

# The process never needs write access to its own image; state lives in /var/lib.
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/newcloud"]
CMD ["serve"]
