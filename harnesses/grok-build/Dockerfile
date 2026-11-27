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

ARG GROK_VERSION=latest
ARG GROK_CLI_VERSION
ARG GROK_CLI_URL_AMD64
ARG GROK_CLI_URL_ARM64
ARG GROK_CLI_SHA256_AMD64
ARG GROK_CLI_SHA256_ARM64

# Pre-seed grok config directories and seed config.
RUN mkdir -p /home/scion/.grok/hooks \
             /home/scion/.grok/skills \
             /home/scion/.grok/plugins \
             /home/scion/.grok/sessions \
             /home/scion/.grok/agents && \
    chown -R scion:scion /home/scion/.grok

COPY --chown=scion:scion home/.grok/config.toml /home/scion/.grok/config.toml

# Install grok CLI binary.
# The official install script downloads to ~/.grok/bin/ and symlinks into
# /usr/local/bin/.  We copy the real binary (cp -L dereferences) so the
# image is self-contained after removing ~/.grok.
# Do NOT switch back to USER scion — sciontool init handles the user switch.
USER root
RUN if [ -n "${GROK_CLI_VERSION}${GROK_CLI_URL_AMD64}${GROK_CLI_URL_ARM64}${GROK_CLI_SHA256_AMD64}${GROK_CLI_SHA256_ARM64}" ]; then \
      case "$(dpkg --print-architecture)" in \
        amd64) URL="${GROK_CLI_URL_AMD64}"; SHA="${GROK_CLI_SHA256_AMD64}" ;; \
        arm64) URL="${GROK_CLI_URL_ARM64}"; SHA="${GROK_CLI_SHA256_ARM64}" ;; \
        *) exit 1 ;; \
      esac && \
      test -n "${GROK_CLI_VERSION}" && test -n "$URL" && test -n "$SHA" && \
      curl -fsSL "$URL" -o /tmp/grok.tar.gz && \
      printf '%s  /tmp/grok.tar.gz\n' "$SHA" | sha256sum -c - && \
      tar -xzf /tmp/grok.tar.gz -C /usr/local/bin grok && \
      chmod +x /usr/local/bin/grok && rm /tmp/grok.tar.gz; \
    else \
    curl -fsSL https://x.ai/cli/install.sh | bash \
    && rm -f /usr/local/bin/grok /usr/local/bin/agent \
    && cp -fL /root/.grok/bin/grok /usr/local/bin/grok \
    && chmod +x /usr/local/bin/grok \
    && rm -rf /root/.grok; \
    fi

CMD ["grok"]
