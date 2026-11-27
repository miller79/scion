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

ARG GEMINI_CLI_VERSION=latest
ARG GEMINI_CLI_URL
ARG GEMINI_CLI_SHA256
ARG GEMINI_CLI_INTEGRITY

# Set `DEVCONTAINER` environment variable to help with orientation
ENV DEVCONTAINER=true

RUN mkdir -p /home/scion/.gemini /home/scion/.gemini/skills && \
  chown -R scion:scion /home/scion/.gemini

# Install gemini-cli and clean up
RUN --mount=type=secret,id=npmrc,target=/root/.npmrc,required=false \
    if [ -n "${GEMINI_CLI_URL}${GEMINI_CLI_SHA256}${GEMINI_CLI_INTEGRITY}" ]; then \
      sh /opt/scion-image-build/lib/install-pinned-npm.sh \
        "${GEMINI_CLI_VERSION}" "${GEMINI_CLI_URL}" "${GEMINI_CLI_SHA256}" "${GEMINI_CLI_INTEGRITY}"; \
    else \
      npm install -g "@google/gemini-cli@${GEMINI_CLI_VERSION}"; \
    fi \
  && npm cache clean --force

# Default entrypoint when none specified
CMD ["gemini"]
