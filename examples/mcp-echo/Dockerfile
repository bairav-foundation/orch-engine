# Copyright 2026 BAIRAV Foundation
# SPDX-License-Identifier: Apache-2.0
#
# bairav/mcp-echo — a tiny reference MCP server image for BAIRAVS
# Skill demos. Stdlib-only, single file, ~55MB.
#
# Build:
#   docker build -t bairav/mcp-echo:v1 examples/mcp-echo
# Push to k3d (make dev's built-in registry):
#   docker tag bairav/mcp-echo:v1 k3d-bairavs-registry:5000/bairav/mcp-echo:v1
#   docker push k3d-bairavs-registry:5000/bairav/mcp-echo:v1
# Deploy:
#   bin/bairav skill deploy k3d-bairavs-registry:5000/bairav/mcp-echo:v1 --name echo

FROM python:3.12-alpine@sha256:b658b05ce9c4eb97ec6fc19424818a1f31993659c2b505a532f470e83807815d

# no pip install — server.py is stdlib-only.
WORKDIR /app
COPY server.py /app/server.py
# Readable by the non-root user whatever the mode in the build context.
RUN chmod 0644 /app/server.py

# Run as a non-root user. Alpine's default numeric UID for a
# throwaway account is fine — nothing on disk needs write access.
RUN adduser -D -u 1000 bairav
USER 1000:1000

EXPOSE 3000
ENV PORT=3000

# HEALTHCHECK mirrors what the K8s readiness probe hits, so
# `docker run` alone also gives you liveness telemetry.
HEALTHCHECK --interval=10s --timeout=2s --start-period=2s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:${PORT}/health || exit 1

ENTRYPOINT ["python3", "/app/server.py"]
