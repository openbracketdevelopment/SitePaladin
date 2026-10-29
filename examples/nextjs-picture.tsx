// App Router server component, not "use client". PUBLIC_VARIANTS=true is required.
// Copy into your app; no Next.js dependency is added to ByteDown.
type Output = { format: string; width: number; height: number; url: string };
type ImageManifest = { format: string; width: number; height: number; outputs: Output[] };

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
