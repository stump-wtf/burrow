# syntax=docker/dockerfile:1
#
# One static burrow binary: modernc SQLite keeps CGO_ENABLED=0, so the build
# needs nothing but the Go toolchain. The runtime image adds git, because the
# Library source clones and refreshes its skills repo at load time
# (internal/skills.refreshGit shells out to git).
#
# DEPLOY SAFETY: the image keeps /usr/local/bin/burrow as its ENTRYPOINT and
# is run with no command override by the StumpCloud edge stack
# (stumpcloud/ansible, playbooks/services/templates/cloud01-edge/compose.yaml.j2), so the
# entrypoint is the deploy contract; existing deployments start the server
# unchanged.

FROM golang:1.27-alpine AS build
WORKDIR /src
# Cache module downloads before copying the full tree.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/burrow ./cmd/burrow

# Named `runtime` so CI's image job passes `no-cache-filter: runtime`: the
# apk upgrade layer must rebuild on every image build, or the Trivy scan (and
# the push behind it) sees whatever package snapshot was current when the
# cached layer was first made. The cairn Dockerfile carries the incident that
# taught this.
FROM alpine:3.24 AS runtime
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates git wget \
    && addgroup -S -g 101 burrow && adduser -S -u 100 -G burrow burrow
COPY --from=build /out/burrow /usr/local/bin/burrow
# UID/GID 100/101, pinned above rather than left to busybox allocation
# order: the deployment creates /volumes/burrow owned by exactly this
# pair, since the host has no burrow user to name.
USER burrow
ENV BURROW_ADDR=:8080
EXPOSE 8080
# The root path serves the sign-in page (internal/httpapi handleRoot) and is
# the liveness signal the server always answers, OIDC configured or not.
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=6 \
    CMD wget -qO- http://127.0.0.1:8080/ >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/usr/local/bin/burrow"]
