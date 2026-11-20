# pggo 无头执行器 worker（Playwright + Chromium + Xvfb）
#   docker build -t pggo-headless ./headless
# 只装完整 Chromium（headful/Xvfb 与「新 headless」共用），不装 chromium-headless-shell。
FROM node:22-bookworm-slim

ENV NODE_ENV=production \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    NPM_CONFIG_UPDATE_NOTIFIER=false

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev \
 && npx playwright install --with-deps --no-shell chromium \
 && apt-get update \
 && apt-get install -y --no-install-recommends xvfb tini \
 && rm -rf /var/lib/apt/lists/* /root/.npm /tmp/*

COPY src ./src

# 非 root 运行；/tmp/.X11-unix 供 Xvfb 使用
RUN useradd -m -u 10001 pggo && mkdir -p /tmp/.X11-unix && chmod 1777 /tmp/.X11-unix
USER pggo

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["node", "src/worker.js"]
