// App Router server component, not "use client". PUBLIC_VARIANTS=true is required.
// Copy into your app; no Next.js dependency is added to ByteDown.
import { setTimeout as sleep } from 'node:timers/promises';

type Output = { format: string; width: number; height: number; url: string };
type ImageManifest = { format: string; width: number; height: number; outputs: Output[] };

// Call only from trusted server-side code after POST /v1/images returns job_id.
export async function waitForByteDownJob(jobID: string): Promise<{ image_id: string }> {
  const base = process.env.BYTEDOWN_INTERNAL_URL;
  const key = process.env.API_KEY;
  if (!base || !key || key.length < 32 || !/^[a-f0-9]{32}$/.test(jobID)) {
    throw new Error('Invalid ByteDown server configuration or job identifier');
  }
  const deadline = Date.now() + 120_000;
  const signal = AbortSignal.timeout(120_000);
  try {
    while (Date.now() < deadline) {
      const response = await fetch(new URL(`/v1/jobs/${jobID}`, base), {
        headers: { Authorization: ['Bearer', key].join(' ') },
        cache: 'no-store', redirect: 'error', signal,
      });
      if (!response.ok) throw new Error('Job unavailable');
      const job = await response.json();
      if (job.status === 'succeeded' && /^[a-f0-9]{32}$/.test(job.image_id)) {
        return { image_id: job.image_id };
      }
      if (job.status !== 'queued' && job.status !== 'processing') {
        throw new Error('Job failed or returned an invalid state');
      }
      await sleep(Math.max(0, Math.min(500, deadline - Date.now())), undefined, { signal });
    }
  } catch {
    // Do not surface backend response bodies, native diagnostics, or credentials.
    throw new Error('ByteDown job failed, became unavailable, or exceeded the polling deadline');
  }
  throw new Error('ByteDown job exceeded the polling deadline');
}

export async function ByteDownPicture({ id, alt }: { id: string; alt: string }) {
  const base = process.env.BYTEDOWN_INTERNAL_URL!;
  const publicBase = process.env.BYTEDOWN_PUBLIC_URL!;
  const response = await fetch(new URL(`/v1/images/${encodeURIComponent(id)}`, base), {
    headers: { Authorization: ['Bearer', process.env.API_KEY!].join(' ') },
    cache: 'no-store',
  });
  if (!response.ok) throw new Error(`ByteDown HTTP ${response.status}`);
  const image: ImageManifest = await response.json();
  const publicURL = (path: string) => {
    // Rewrite only service-owned relative media paths, never originals.
    if (!/^\/media\/[a-zA-Z0-9_-]+$/.test(path)) throw new Error('Invalid variant URL');
    return new URL(path, publicBase).href;
  };
  const outputs = (format: string) => image.outputs
    .filter(output => output.format === format)
    .sort((a, b) => a.width - b.width);
  const srcSet = (format: string) => outputs(format)
    .map(output => `${publicURL(output.url)} ${output.width}w`).join(', ');
  const fallback = outputs(image.format).at(-1);
  if (!fallback) throw new Error('Optimized original-format output is not ready');
  const sizes = '(max-width: 960px) 100vw, 960px';
  return (
    <picture>
      {['avif', 'webp'].map(format => outputs(format).length > 0 && (
        <source key={format} type={`image/${format}`} srcSet={srcSet(format)} sizes={sizes} />
      ))}
      {/* Plain img intentionally avoids Next Image double optimization. */}
      <img src={publicURL(fallback.url)} srcSet={srcSet(image.format)} sizes={sizes}
        width={fallback.width} height={fallback.height} alt={alt} loading="lazy"
        style={{ maxWidth: '100%', height: 'auto' }} />
    </picture>
  );
}
