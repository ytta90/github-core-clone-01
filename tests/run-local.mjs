// Local dev launcher: start the forge on a fixed port so sdk/run.ts can dial it.
// This is a development tool only; the trial runs main.ts instead.
// Usage: MAKEFUN_SHOP_TOKEN=local-token node --experimental-strip-types tests/run-local.mjs
process.env.MAKEFUN_SHOP_TOKEN ??= 'local-token';
const { createApp } = await import('../app.ts');
const port = Number(process.env.LOCAL_PORT ?? '18123');
const server = createApp();
await new Promise((resolve) => server.listen(port, resolve));
console.log(`git-forge listening on http://127.0.0.1:${port} (token ${process.env.MAKEFUN_SHOP_TOKEN})`);
