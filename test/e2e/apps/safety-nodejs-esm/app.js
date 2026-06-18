// ES module HTTP server. "type": "module" in package.json makes this file an
// ESM module. The app exists to verify that --require register.js (injected via
// NODE_OPTIONS by libotelinject.so) does not crash an ESM application.
import { createServer } from 'http';

const server = createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'text/plain' });
    res.end('hello from esm node\n');
});

server.listen(8080, () => {
    console.log('listening on 8080');
});
