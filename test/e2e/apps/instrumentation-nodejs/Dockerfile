FROM node:20.20.2-slim
WORKDIR /app
COPY package.json .
RUN npm install
COPY app.js .
CMD ["node", "app.js"]
