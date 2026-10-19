// Use the same @/ imports as Next without requiring a browser or a database.
const path = require('node:path');
const Module = require('node:module');
const resolve = Module._resolveFilename;
Module._resolveFilename = function (request, ...args) {
  return resolve.call(this, request.startsWith('@/') ? path.join(__dirname, '../src', request.slice(2)) : request, ...args);
};
require('ts-node').register({transpileOnly: true, compilerOptions: {module:'CommonJS', moduleResolution:'node'}});
