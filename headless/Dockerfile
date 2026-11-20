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

# 非 root 运行（Playwright 下 Chromium 带 --no-sandbox，必须避免 root）。
# 主组为 0：中枢把 Cookie 推送写成 root:root 0640，只读挂载后同组可读。
# /state 存轮换后的 Cookie（挂载命名卷时 Docker 会沿用这里的属主与权限）。
# /tmp/.X11-unix 供 Xvfb 使用。
RUN useradd -m -u 10001 -g 0 pggo \
 && mkdir -p /state /tmp/.X11-unix \
 && chown 10001:0 /state && chmod 0700 /state \
 && chmod 1777 /tmp/.X11-unix
USER 10001:0

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["node", "src/worker.js"]
