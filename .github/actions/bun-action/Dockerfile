FROM oven/bun:1-alpine@sha256:d888c0ae6c86d7866ff10c5aafdd9077b36aee6455b33dd270fb93c0dd5cef6f
# The upstream image includes a node -> bun fallback alias.
RUN apk upgrade --no-cache \
    && rm /usr/local/bun-node-fallback-bin/node \
    && bun -e 'if (["node", "npm", "npx"].some(command => Bun.which(command))) throw new Error("CI image must expose only Bun")'
ENTRYPOINT ["/usr/local/bin/bun"]
