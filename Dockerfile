# claude-launcher with what Claude needs to work on projects: git, curl and an
# SSH client. Two folders are worth mounting: /projects, and /home/claude,
# where the page installs Claude Code and gh and where the sign-in is kept.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /claude-launcher .

FROM debian:13-slim
# tini adopts and reaps the processes Claude leaves behind
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git openssh-client procps tini \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1000 claude \
 && install -d -o claude -g claude /projects
COPY --from=build /claude-launcher /usr/local/bin/claude-launcher
USER claude
ENV LANG=C.UTF-8 LAUNCHER_ADDR=:8787
WORKDIR /projects
EXPOSE 8787
ENTRYPOINT ["tini", "--", "claude-launcher"]
