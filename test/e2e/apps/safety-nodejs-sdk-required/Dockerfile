FROM node:24.18.0-slim@sha256:6f7b03f7c2c8e2e784dcf9295400527b9b1270fd37b7e9a7285cf83b6951452d
WORKDIR /app
COPY package.json .
RUN npm install
COPY app.js otel-init.js ./
CMD ["node", "--require", "/app/otel-init.js", "app.js"]
