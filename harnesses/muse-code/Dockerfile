# syntax=docker/dockerfile:1
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG BASE_IMAGE
FROM ${BASE_IMAGE}
ARG MUSE_CLI_VERSION
ARG MUSE_CLI_URL_AMD64
ARG MUSE_CLI_URL_ARM64
ARG MUSE_CLI_SHA256_AMD64
ARG MUSE_CLI_SHA256_ARM64

# Image is immutable; update muse by rebuilding, not by runtime auto-update.
ENV MUSE_NO_AUTO_UPDATE=1

# Installer is bash-only ([[ ]]); download to a file rather than piping to
# bash so a failed curl aborts the build instead of silently running nothing.
# Installs straight into /usr/local/bin (on PATH for scion) via the
# installer's supported MUSE_INSTALL_DIR override.
RUN if [ -n "${MUSE_CLI_VERSION}${MUSE_CLI_URL_AMD64}${MUSE_CLI_URL_ARM64}${MUSE_CLI_SHA256_AMD64}${MUSE_CLI_SHA256_ARM64}" ]; then \
      case "$(dpkg --print-architecture)" in \
        amd64) URL="${MUSE_CLI_URL_AMD64}"; SHA="${MUSE_CLI_SHA256_AMD64}" ;; \
        arm64) URL="${MUSE_CLI_URL_ARM64}"; SHA="${MUSE_CLI_SHA256_ARM64}" ;; \
        *) exit 1 ;; \
      esac && \
      test -n "${MUSE_CLI_VERSION}" && test -n "$URL" && test -n "$SHA" && \
      curl -fsSL "$URL" -o /tmp/muse.tar.gz && \
      printf '%s  /tmp/muse.tar.gz\n' "$SHA" | sha256sum -c - && \
      tar -xzf /tmp/muse.tar.gz -C /usr/local/bin muse && \
      chmod +x /usr/local/bin/muse && rm /tmp/muse.tar.gz; \
    else \
    curl -fsSL -o /tmp/muse-install.sh https://dev.meta.ai/install.sh && \
    MUSE_INSTALL_DIR=/usr/local/bin MUSE_NO_MODIFY_PATH=1 bash /tmp/muse-install.sh && \
    rm /tmp/muse-install.sh && \
    muse --version; \
    fi

# Create config and data directories owned by agent user
RUN mkdir -p /home/scion/.config/muse \
             /home/scion/.local/share/muse \
             /home/scion/.muse/skills && \
    chown -R scion:scion /home/scion/.config \
                         /home/scion/.local \
                         /home/scion/.muse

CMD ["muse"]
