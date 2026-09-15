ARG ALPINE_IMAGE=alpine:3@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
ARG BUN_IMAGE=oven/bun:1-alpine@sha256:d888c0ae6c86d7866ff10c5aafdd9077b36aee6455b33dd270fb93c0dd5cef6f

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
VOLUME ["/data"]
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q --spider http://localhost:3000/api/v1/health || exit 1

CMD ["bun", "packages/backend/dist/index.js"]
