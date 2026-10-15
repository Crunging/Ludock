ARG ALPINE_IMAGE=alpine:3@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG BUN_IMAGE=oven/bun:1-alpine@sha256:629e17411f1f129dbec3af78d5af9c9f2a937435c80349437206c6b0b7422373

# Build JavaScript artifacts on the native build platform.
FROM --platform=$BUILDPLATFORM ${BUN_IMAGE} AS deps
WORKDIR /app
COPY package.json bun.lock bunfig.toml .bun-version ./
COPY scripts/ci/check-bun.mjs scripts/ci/check-bun.mjs
COPY packages/backend/package.json packages/backend/
COPY packages/frontend/package.json packages/frontend/
COPY packages/shared/package.json packages/shared/
RUN bun scripts/ci/check-bun.mjs && bun install --frozen-lockfile --linker=isolated

COPY packages/shared/src/ packages/shared/src/

FROM deps AS build-frontend
COPY packages/frontend/src/ packages/frontend/src/
COPY packages/frontend/public/ packages/frontend/public/
COPY packages/frontend/scripts/ packages/frontend/scripts/
# Preview and production share the asset handler; frontend tooling type-checks it.
COPY packages/backend/src/static-files.ts packages/backend/src/static-files.ts
COPY packages/frontend/index.html packages/frontend/tsconfig*.json packages/frontend/
RUN bun run --filter @ludock/frontend build

FROM deps AS build-backend
# Published attestations also record the installed inputs to the bundled code.
ARG BUILDKIT_SBOM_SCAN_STAGE=true
COPY packages/backend/src/ packages/backend/src/
# The bundle step type-checks against the backend's compiler settings.
COPY tsconfig.base.json ./
COPY packages/backend/tsconfig.json packages/backend/
COPY scripts/build-backend.mjs scripts/build-backend.mjs
RUN bun run --filter @ludock/backend build

# The runtime executable must match the target platform.
FROM ${BUN_IMAGE} AS bun-runtime

FROM ${ALPINE_IMAGE} AS runtime
RUN apk upgrade --no-cache \
    && apk add --no-cache libstdc++ docker-cli docker-cli-compose
WORKDIR /app

COPY --from=bun-runtime /usr/local/bin/bun /usr/local/bin/bun

# Includes shared chunks, source maps, dependency inventory, and license notices.
COPY --from=build-backend /app/packages/backend/dist packages/backend/dist/
COPY --from=build-frontend /app/packages/frontend/dist packages/frontend/dist/

ENV NODE_ENV=production
ENV PORT=3000
ENV LUDOCK_DB_PATH=/data/ludock.db
ENV LUDOCK_BACKUP_ROOTS=/backups
VOLUME ["/data"]
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q --spider http://localhost:3000/api/v1/health || exit 1

# The server never turns strings into code. Helper containers reuse this image
# and run `bun -e`, so the flag stays on this command instead of BUN_OPTIONS.
CMD ["bun", "--disallow-code-generation-from-strings=strict", "packages/backend/dist/index.js"]
