'use strict';

const http = require('node:http');

function newRound() {
  return { checked: 0, waiters: new Set() };
}

function release(round) {
  for (const finish of round.waiters) finish();
}

function rendezvous(round, response) {
  return new Promise((resolve) => {
    function finish() {
      round.waiters.delete(finish);
      response.off('close', finish);
      resolve();
    }

    // A disconnected client must not leave its response retained in the round.
    round.waiters.add(finish);
    response.once('close', finish);
    if (round.checked === 2) release(round);
  });
}

function json(response, status, value) {
  response.writeHead(status, { 'Content-Type': 'application/json' });
  response.end(JSON.stringify(value) + '\n');
}

function reject(response, reason) {
  response.writeHead(409, { 'Content-Type': 'text/plain; charset=utf-8' });
  response.end(reason + '\n');
}

// Each server owns its stock and rendezvous; importing this file opens no port.
function createServer() {
  let stock = 1;
  let round = newRound();

  async function purchase(response) {
    if (stock <= 0) {
      reject(response, 'purchase rejected: stock is unavailable');
      return;
    }
    const checkedRound = round;
    if (checkedRound.checked >= 2) {
      reject(response, 'purchase rejected: two purchases are already in progress');
      return;
    }
    checkedRound.checked++;

    // Intentionally yield between the check and decrement. Both purchases
    // must check the same unit before either resumes, even on one event loop.
    await rendezvous(checkedRound, response);
    if (response.destroyed) return;
    if (round !== checkedRound) {
      reject(response, 'purchase rejected: inventory was reset');
      return;
    }
    stock--;
    json(response, 201, { accepted: true });
  }

  const methods = {
    '/reset': 'POST',
    '/purchase': 'POST',
    '/state': 'GET',
    '/available-stock': 'GET',
  };

  return http.createServer({ headersTimeout: 5000 }, (request, response) => {
    request.resume();
    const path = new URL(request.url, 'http://127.0.0.1').pathname;
    if (!Object.hasOwn(methods, path)) {
      response.writeHead(404);
      response.end('404 page not found\n');
      return;
    }
    const method = methods[path];
    const observation = method === 'GET';
    if (request.method !== method && !(observation && request.method === 'HEAD')) {
      response.writeHead(405, { Allow: observation ? 'GET, HEAD' : method });
      response.end('Method Not Allowed\n');
      return;
    }

    switch (path) {
      case '/reset': {
        const oldRound = round;
        stock = 1;
        round = newRound();
        release(oldRound);
        response.writeHead(204);
        response.end();
        break;
      }
      case '/purchase':
        purchase(response).catch((error) => {
          console.error('Inventory purchase failed:', error);
          response.destroy();
        });
        break;
      case '/state':
        json(response, 200, { stock });
        break;
      case '/available-stock':
        json(response, 200, { stock: Math.max(stock, 0) });
        break;
    }
  });
}

module.exports = { createServer };

if (require.main === module) {
  const server = createServer();
  server.on('error', (error) => {
    console.error('Inventory example stopped:', error.message);
    process.exitCode = 1;
  });
  server.listen(8080, '127.0.0.1', () => {
    console.log('Deliberately vulnerable inventory is ready at http://127.0.0.1:8080');
    console.log('Use it only for the local ConcurTest demonstration.');
  });
}
