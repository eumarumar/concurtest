'use strict';

const assert = require('node:assert/strict');
const { once } = require('node:events');
const http = require('node:http');
const test = require('node:test');
const { createServer } = require('./server');

async function startServer(t) {
  const server = createServer();
  t.after(async () => {
    const closed = once(server, 'close');
    server.close();
    server.closeAllConnections();
    await closed;
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  return server;
}

function request(server, method, path) {
  return new Promise((resolve, reject) => {
    const client = http.request({
      host: '127.0.0.1', port: server.address().port, method, path, agent: false,
    }, (response) => {
      let body = '';
      response.setEncoding('utf8');
      response.on('data', (chunk) => { body += chunk; });
      response.on('error', reject);
      response.on('end', () => resolve({
        status: response.statusCode, contentType: response.headers['content-type'], body,
      }));
    });
    client.on('error', reject);
    client.setTimeout(2000, () => client.destroy(new Error('request timed out')));
    client.end();
  });
}

test('two purchases repeatably oversell one unit and reset restores it', { timeout: 5000 }, async (t) => {
  const server = await startServer(t);
  for (let trial = 0; trial < 10; trial++) {
    const reset = await request(server, 'POST', '/reset');
    assert.equal(reset.status, 204);
    assert.equal(reset.body, '');
    assert.deepEqual(await request(server, 'GET', '/state'), {
      status: 200, contentType: 'application/json', body: '{"stock":1}\n',
    });

    const purchases = await Promise.all([
      request(server, 'POST', '/purchase'), request(server, 'POST', '/purchase'),
    ]);
    for (const purchase of purchases) {
      assert.deepEqual(purchase, {
        status: 201, contentType: 'application/json', body: '{"accepted":true}\n',
      });
    }
    assert.equal((await request(server, 'GET', '/state')).body, '{"stock":-1}\n');
    assert.equal((await request(server, 'GET', '/available-stock')).body, '{"stock":0}\n');
    const rejected = await request(server, 'POST', '/purchase');
    assert.equal(rejected.status, 409);
    assert.equal(rejected.body, 'purchase rejected: stock is unavailable\n');
  }
});

test('reset releases a waiting purchase without changing the new stock', { timeout: 5000 }, async (t) => {
  const server = await startServer(t);
  const reached = once(server, 'request');
  const pending = request(server, 'POST', '/purchase');
  // The request event fires after the handler has entered the rendezvous.
  await reached;
  assert.equal((await request(server, 'GET', '/state')).body, '{"stock":1}\n');
  await request(server, 'POST', '/reset');
  const rejected = await pending;
  assert.equal(rejected.status, 409);
  assert.equal(rejected.body, 'purchase rejected: inventory was reset\n');
  assert.equal((await request(server, 'GET', '/state')).body, '{"stock":1}\n');
  const purchases = await Promise.all([
    request(server, 'POST', '/purchase'), request(server, 'POST', '/purchase'),
  ]);
  assert.deepEqual(purchases.map((purchase) => purchase.status), [201, 201]);
});

test('a disconnected purchase does not decrement stock when its partner arrives', { timeout: 5000 }, async (t) => {
  const server = await startServer(t);
  const reached = once(server, 'request');
  const client = http.request({
    host: '127.0.0.1', port: server.address().port, method: 'POST', path: '/purchase', agent: false,
  });
  const failed = once(client, 'error');
  client.end();
  const [, response] = await reached;
  const disconnected = once(response, 'close');
  client.destroy();
  const [error] = await failed;
  assert.equal(error.code, 'ECONNRESET');
  await disconnected;
  assert.equal((await request(server, 'POST', '/purchase')).status, 201);
  assert.equal((await request(server, 'GET', '/state')).body, '{"stock":0}\n');
});

test('routes use the same methods as the Go demo', { timeout: 5000 }, async (t) => {
  const server = await startServer(t);
  for (const [method, path, status] of [
    ['GET', '/reset', 405], ['GET', '/purchase', 405],
    ['POST', '/state', 405], ['POST', '/available-stock', 405],
    ['GET', '/missing', 404], ['GET', '/toString', 404],
    ['HEAD', '/state', 200], ['HEAD', '/available-stock', 200],
  ]) {
    assert.equal((await request(server, method, path)).status, status, `${method} ${path}`);
  }
});
