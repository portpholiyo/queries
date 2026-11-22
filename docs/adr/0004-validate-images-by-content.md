# 0004. Validate images by content, reject SVG, strip metadata

- Status: Accepted
- Date: 2026-11-22

## Context
Visitors upload files that are later stored, forwarded to Slack and served back to browsers. File names and declared content types are controlled by the sender, SVG can carry script, and phone photos often embed GPS coordinates.

## Options considered
1. Trust the extension or declared content type: trivially bypassed.
2. Accept any file and rely on serving headers: still stores and forwards untrusted content.
3. Identify the format from the bytes, allow a fixed list of raster formats, strip metadata where it is safe to do so, and serve defensively.

## Decision
Option 3. Formats are recognised by signature (PNG, JPEG, GIF, WebP, AVIF, HEIC, BMP, TIFF); everything else, including SVG, is rejected. JPEG EXIF and XMP segments are removed before storage. Responses use `nosniff`, a CSP of `default-src 'none'; sandbox`, and inline display only for formats browsers render safely, otherwise as an attachment.

## Consequences
- Script-bearing and disguised files never reach storage, Slack or other visitors.
- Visitors' location metadata is not retained.
- Some legitimate vector files are refused; widening the list needs a security review.
- Metadata stripping is implemented for JPEG only; other formats are stored as received.
