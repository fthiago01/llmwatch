const VERSION = 'llmwatch-v1';
const CACHE_NAME = VERSION;
const SKELETON = [
  '/manifest.webmanifest',
  '/icons/icon.svg',
  '/icons/icon-192.png',
  '/icons/icon-512.png'
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then((cache) => cache.addAll(SKELETON))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((names) => Promise.all(
        names
          .filter((name) => name !== VERSION)
          .map((name) => caches.delete(name))
      ))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const request = event.request;
  const url = new URL(request.url);

  // API e WebSocket são dados ao vivo; nunca devem servir conteúdo antigo.
  if (request.method !== 'GET' || url.origin !== self.location.origin) {
    return;
  }

  if (url.pathname.startsWith('/api/') || url.pathname === '/ws') {
    return;
  }

  // Páginas autenticadas e administrativas contêm dados sensíveis ou pessoais.
  if (url.pathname === '/login' || url.pathname === '/password' || url.pathname === '/admin') {
    return;
  }

  if (request.headers.has('Upgrade')) {
    return;
  }

  if (SKELETON.includes(url.pathname)) {
    event.respondWith(
      caches.match(request).then((cached) => {
        if (cached) {
          return cached;
        }
        return fetch(request).then((response) => {
          if (!response || response.status !== 200) {
            return response;
          }
          const copy = response.clone();
          caches.open(CACHE_NAME).then((cache) => cache.put(request, copy));
          return response;
        });
      })
    );
    return;
  }

  if (request.mode === 'navigate') {
    event.respondWith(
      fetch(request).then((response) => {
        if (!response || response.status !== 200) {
          return response;
        }
        return response;
      }).catch(() => new Response(
        '<!doctype html><html lang="pt-BR"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>llmwatch — offline</title><style>body{font-family:system-ui,sans-serif;background:#0f172a;color:#f8fafc;display:grid;min-height:100vh;margin:0;place-items:center}main{max-width:32rem;padding:2rem;text-align:center}button{margin-top:1rem;padding:.75rem 1.25rem;border:0;border-radius:.5rem;background:#4f46e5;color:#fff;font-size:1rem;cursor:pointer}</style></head><body><main><h1>llmwatch está offline — verifique se o serviço está rodando</h1><button type="button" onclick="location.reload()">Tentar de novo</button></main></body></html>',
        {
          status: 503,
          headers: { 'Content-Type': 'text/html; charset=utf-8' }
        }
      ))
    );
    return;
  }

  // Demais recursos passam diretamente pela rede.
});
