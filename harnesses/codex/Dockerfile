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

ARG CODEX_CLI_VERSION=latest
ARG CODEX_CLI_URL
ARG CODEX_CLI_SHA256
ARG CODEX_CLI_INTEGRITY

RUN mkdir -p /home/scion/.codex /home/scion/.codex/skills && \
  chown -R scion:scion /home/scion/.codex

# install codex, expose it on standard shell PATH, and clean up
RUN --mount=type=secret,id=npmrc,target=/root/.npmrc,required=false \
    if [ -n "${CODEX_CLI_URL}${CODEX_CLI_SHA256}${CODEX_CLI_INTEGRITY}" ]; then \
      sh /opt/scion-image-build/lib/install-pinned-npm.sh \
        "${CODEX_CLI_VERSION}" "${CODEX_CLI_URL}" "${CODEX_CLI_SHA256}" "${CODEX_CLI_INTEGRITY}"; \
    else \
      npm install -g "@openai/codex@${CODEX_CLI_VERSION}"; \
    fi \
  && ln -sf /usr/local/share/npm-global/bin/codex /usr/local/bin/codex \
  && npm cache clean --force

# default entrypoint when none specified
CMD ["codex"]
