# ByteDown

**Smaller files. More possibilities.**

ByteDown is an MIT-licensed, self-hosted Go image service for one operator and
multiple trusted server-side applications. It preserves JPEG/PNG uploads and
asynchronously creates a full-size optimized copy and responsive WebP/AVIF
variants. It is an initial release, not a production-readiness guarantee.

Correctness, understandable Go, durable work, privacy, and simple deployment take
priority over maximum compression. Lossy JPEG/WebP/AVIF quality settings are
independent. Outputs can be larger than their source; resized savings include
resizing. Originals are never silently substituted for generated outputs.

The default web profile uses widths 480, 960, 1440, and 1920 plus source width,
without cropping or upscaling. Only static JPEG and PNG are accepted. Generated
outputs are oriented, converted to sRGB, and stripped of unnecessary metadata.
Unsupported color configurations must fail, not be guessed.

Future milestones: operational hardening and load evaluation; Craft/Payload
integrations; optional completion webhooks; S3-compatible storage; reviewed new
image formats. Accounts, billing, dashboards, public signup, remote URL imports,
SVG/GIF, PDF/OCR, archives, video, Kubernetes, and microservices are out of scope.
PDF/OCR/archive processing would be separate modules, not image-codec extensions.
