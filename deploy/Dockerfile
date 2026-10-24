# Self-check, then hand off to a tiny runtime image.
#
# Two build stages: the browser client first, then the Go binary that embeds it. Node never
# reaches the runtime image, and nothing from npm ships inside the binary — the bundle is
# static bytes compiled in, so the deployed artifact is still one file.
#
# CGO is disabled so the binary is static: the runtime stage does not need glibc, and
# cross-compiling from a workstation keeps working.
#
# The Go version must match go.mod, which requires 1.26: os.Root's write methods
# (MkdirAll, Rename, RemoveAll) only exist from 1.25 onward and are what gives the
# tree layout its syscall-level path containment. Building with an older toolchain
# fails, and pinning it too low here is a build break that only shows up on deploy.
FROM node:24-bookworm AS web
WORKDIR /web

# Dependencies come from the lockfile alone, so this layer is cached until the lockfile
# changes rather than on every source edit.
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile

COPY web/ ./
# The Go embed directive needs the output directory to exist at compile time; Vite writes
# into it (one level up from /web) and empties it first.
RUN mkdir -p /internal/webassets/dist && pnpm build

FROM golang:1.26-bookworm AS build
WORKDIR /src

# Cache the module graph separately from the source so edits do not re-download it.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Replace the committed bundle with one built from the TypeScript in this commit, so the
# image can never ship a frontend that disagrees with its own source.
COPY --from=web /internal/webassets/dist ./internal/webassets/dist

# Run the tests inside the build: a failing test should stop the image from being
# produced, not sit in a report nobody reads before the deploy.
RUN CGO_ENABLED=0 go vet ./... && \
    CGO_ENABLED=0 go test ./... -count=1 -timeout 300s

ENV CGO_ENABLED=0 GOOS=linux
# The version is a build argument because the build context carries no usable .git state:
# without -X the binary reports "dev", and an image whose version is "dev" cannot be matched
# to a commit during an incident. Pass it from CI or from docker compose.
ARG VERSION=dev
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/newcloud ./cmd/newcloud

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/newcloud /newcloud

# The process never needs write access to its own image; state lives in /var/lib.
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/newcloud"]
CMD ["serve"]
