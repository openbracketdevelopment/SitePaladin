// Node.js 22+ server-side only. Never import this module into browser code.
import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

export async function uploadAndWait(bytes, filename = 'image.png', profile = 'web') {
  const base = new URL(process.env.BYTEDOWN_URL || 'http://127.0.0.1:8080');
  const key = process.env.API_KEY;
  if (!key || key.length < 32) throw new Error('API_KEY must contain at least 32 characters');
  const signal = AbortSignal.timeout(240_000);
  async function request(path, options = {}) {
    const url = new URL(path, base);
    if (url.origin !== base.origin) throw new Error('Cross-origin credential forwarding refused');
    const response = await fetch(url, {
      ...options, signal, redirect: 'error',
      headers: { ...options.headers, Authorization: ['Bearer', key].join(' ') },
    });
    if (!response.ok) throw new Error(`ByteDown HTTP ${response.status}`);
    return response.json();
  }
  const form = new FormData();
  form.set('file', new Blob([bytes]), filename);
  form.set('profile', profile);
  const accepted = await request('/v1/images', { method: 'POST', body: form });
  let job;
  for (;;) {
    job = await request(accepted.status_url);
    if (job.status === 'succeeded') break;
    if (job.status === 'failed') throw new Error(`ByteDown job failed: ${job.error_code}`);
    signal.throwIfAborted();
    await new Promise(resolve => setTimeout(resolve, 500));
  }
  const image = await request(`/v1/images/${accepted.image_id}`);
  return { image, job };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const filename = process.argv[2];
  if (!filename) throw new Error('Usage: node examples/server-upload.mjs image.png');
  console.log(JSON.stringify(await uploadAndWait(await readFile(filename), 'sample'), null, 2));
}
