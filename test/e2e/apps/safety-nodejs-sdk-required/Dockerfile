FROM node:24.21.0-slim@sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6
WORKDIR /app
COPY package.json .
RUN npm install
COPY app.js otel-init.js ./
CMD ["node", "--require", "/app/otel-init.js", "app.js"]
