FROM node:16.20.2-slim@sha256:3ebf2875c188d22939c6ab080cfb1a4a6248cc86bae600ea8e2326aa03acdb8f
WORKDIR /app
COPY package.json .
RUN npm install
COPY app.js .
CMD ["node", "app.js"]
