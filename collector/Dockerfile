# syntax=docker/dockerfile:1

# The collector links NVIDIA's NVML through cgo, so it is a dynamically linked
# glibc binary. It is built on AlmaLinux 8 (the RHEL 8 rebuild, glibc 2.28,
# supported until 2029): the binary then only needs glibc >= 2.28 and runs on
# RHEL/Rocky/Alma 8+, Ubuntu 20.04+, Debian 10+. The build fails if a newer
# glibc symbol ever sneaks in.
#
# go-nvml relies on GNU ld (it leaves NVML symbols unresolved and binds them
# lazily after dlopen), so this uses the regular GNU toolchain. The build stage
# runs on the target platform: the other architecture builds under emulation.
#   docker buildx build --platform linux/amd64,linux/arm64 -t <image> .
#   docker buildx build --platform linux/amd64,linux/arm64 --target artifacts --output dist .

FROM almalinux:8 AS build
ARG TARGETARCH
ARG GO_VERSION=1.27.2
ARG GO_SHA256_amd64=ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5
ARG GO_SHA256_arm64=94f3e30b8e374bc285e7dadc11e0865726b9bc6e85b841ccceaabc0214c6b7c8
# Oldest glibc the binary must run on.
ARG GLIBC=2.28

RUN set -eux; \
    dnf -y install --setopt=install_weak_deps=False gcc binutils tar gzip; \
    dnf clean all; \
    eval "sha=\$GO_SHA256_${TARGETARCH}"; \
    curl -fsSLo /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz"; \
    echo "${sha}  /tmp/go.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH \
    GOTOOLCHAIN=local \
    CGO_CFLAGS=-Wno-deprecated-declarations

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . .

ARG VERSION=dev
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    mkdir -p /out; \
    CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/klique-idle-collector ./cmd/klique-idle-collector; \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/mock-receiver ./cmd/mock-receiver; \
    need="$(readelf -W --dyn-syms /out/klique-idle-collector | grep -o 'GLIBC_[0-9.]*' | sed 's/GLIBC_//' | sort -uV | tail -n1)"; \
    echo "klique-idle-collector (${TARGETARCH}) needs glibc >= ${need}"; \
    if [ "$(printf '%s\n%s\n' "$need" "$GLIBC" | sort -V | tail -n1)" != "$GLIBC" ]; then \
      echo "binary requires glibc ${need}, newer than the supported ${GLIBC}" >&2; exit 1; \
    fi

# Plain binaries for bare-metal / Slurm / PBS installs.
FROM scratch AS artifacts
COPY --from=build /out/ /

# Distroless: glibc and CA certificates, no shell, runs as non-root (65532).
# Debug with: kubectl exec <pod> -- klique-idle-collector --once
FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/klique-idle-collector /out/mock-receiver /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/klique-idle-collector"]
