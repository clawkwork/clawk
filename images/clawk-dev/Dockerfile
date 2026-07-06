# clawk-dev — the default rootfs for clawk OCI sandboxes.
#
# Same spirit as docker/sandbox-templates:claude-code (node + claude +
# the tools coding agents lean on), with one deliberate difference:
# NO user is created here. clawk-init creates the `agent` user at boot
# with the HOST's uid/gid, so virtio-fs worktree ownership lines up
# between host and guest — a baked-in uid 1000 would fight that.
#
# Likewise no init system, sshd, or cloud-init: clawk-init is PID 1 and
# the vsock pty-agent is the only transport. CMD/ENTRYPOINT are ignored
# by clawk; the image is purely a filesystem + Env contract.
#
# Build & use WITHOUT a registry (from the repo root):
#
#	docker build -t clawk-dev images/clawk-dev
#	docker save clawk-dev -o ~/clawk-dev.tar
#
#	# clawk.mod
#	vm (
#	    image ~/clawk-dev.tar
#	)
#
# Or publish (any registry, including `docker run -d -p 5000:5000
# registry:2` on localhost):
#
#	docker build -t ghcr.io/clawkwork/clawk-dev:v0 images/clawk-dev
#	docker push ghcr.io/clawkwork/clawk-dev:v0
#
#	vm (
#	    image ghcr.io/clawkwork/clawk-dev:v0
#	)

FROM golang:1.25-bookworm AS go

FROM node:22-bookworm-slim

# Go toolchain, lifted from the official image (no apt repo dance).
# The PATH addition rides the image config Env, which clawk bakes into
# the rootfs at disk-build time.
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH

# Coding agents. claude is what bare `clawk` attaches to; codex is the
# other built-in runner.
#
# The global npm prefix is relocated to /usr/local/npm-global and made
# world-writable so claude/codex can self-update at runtime. Why not just
# install into the default /usr/local prefix: that tree is root-owned,
# and clawk-init creates the `agent` user with the HOST's (dynamic) uid
# at boot — so there's no build-time uid to chown to, and the updater
# fails with "no write permission to npm prefix". A dedicated writable
# prefix keeps the rest of /usr/local/bin root-owned while letting the
# agent (which already has passwordless sudo, so this widens no real
# boundary) update its own coding agents. The Env rides the image config,
# which clawk bakes into the rootfs (machine/oci ImageEnvPath); it must
# precede the install so the global packages land in the new prefix.
ENV NPM_CONFIG_PREFIX=/usr/local/npm-global
ENV PATH=/usr/local/npm-global/bin:$PATH
RUN mkdir -p /usr/local/npm-global \
    && npm install -g @anthropic-ai/claude-code @openai/codex \
    && npm cache clean --force \
    && chmod -R a+rwX /usr/local/npm-global

# The toolbox agents reach for constantly. build-essential is the
# heaviest entry (~250 MB) but native node modules, cgo, and "just make
# it" workflows all need a C toolchain; drop it if you want a slimmer
# variant. sudo is included for user scripts' benefit — clawk itself
# runs root work through the agent and never needs it. sox is the audio
# recorder Claude Code's voice dictation falls back to on Linux; it reads
# the virtio-snd mic (CLAWK_AUDIO_INPUT, on by default) via ALSA.
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        git \
        jq \
        less \
        openssh-client \
        procps \
        ripgrep \
        sox \
        sudo \
        unzip \
        vim-tiny \
        wget \
    && rm -rf /var/lib/apt/lists/*

# GitHub CLI — agents use it for PR workflows; not packaged by Debian.
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

ENV LANG=C.UTF-8
