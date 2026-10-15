# syntax=docker/dockerfile:1

# The build output is plain JavaScript with no native modules, so the build
# stage always runs on the build machine's own platform. Multi-arch builds
# don't have to run npm under emulation.
FROM --platform=$BUILDPLATFORM node:25-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json ./
COPY src ./src
RUN npm test && npm prune --omit=dev && rm -rf dist/test dist/scripts
# Release builds pass the version from the Git tag (e.g. 1.2.3).
ARG VERSION
RUN if [ -n "$VERSION" ]; then npm pkg set version="$VERSION"; fi

FROM node:25-alpine
LABEL org.opencontainers.image.title="proxmox-mcp" \
      org.opencontainers.image.description="MCP server for Proxmox VE" \
      org.opencontainers.image.source="https://github.com/mattoddie/proxmox-mcp" \
      org.opencontainers.image.documentation="https://github.com/mattoddie/proxmox-mcp#readme" \
      org.opencontainers.image.licenses="MIT"
ENV NODE_ENV=production \
    MCP_TRANSPORT=http \
    MCP_HTTP_HOST=0.0.0.0 \
    MCP_HTTP_PORT=8080
WORKDIR /app
COPY --from=build --chown=node:node /app/package.json ./
COPY --from=build --chown=node:node /app/node_modules ./node_modules
COPY --from=build --chown=node:node /app/dist ./dist
USER node
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["node", "-e", "if (process.env.MCP_TRANSPORT === 'stdio') process.exit(0); fetch('http://127.0.0.1:' + process.env.MCP_HTTP_PORT + '/health').then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))"]
ENTRYPOINT ["node", "dist/index.js"]
