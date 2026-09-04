/**
 * A ustar reader, so the tar test asserts on the ARCHIVE rather than on the
 * list of entries the writer thought it wrote.
 *
 * Independent of `src/tar.ts` on purpose: a bug shared between a writer and a
 * reader written together cancels out, and the header checksum is exactly the
 * kind of thing that would.
 */

export interface Entry {
  name: string
  mode: number
  size: number
  type: string
  linkTarget: string
  body: Buffer
  checksumOK: boolean
}

export function untar(archive: Buffer): Entry[] {
  const entries: Entry[] = []
  for (let offset = 0; offset + 512 <= archive.length; ) {
    const header = archive.subarray(offset, offset + 512)
    if (header.every((b) => b === 0)) break
    const name = str(header, 0, 100)
    const prefix = str(header, 345, 155)
    const size = parseInt(str(header, 124, 12).trim() || '0', 8)
    const stored = parseInt(str(header, 148, 8).trim().replace(/\0/g, '') || '0', 8)

    const zeroed = Buffer.from(header)
    zeroed.fill(0x20, 148, 156)
    let sum = 0
    for (const byte of zeroed) sum += byte

    entries.push({
      name: prefix ? `${prefix}/${name}` : name,
      mode: parseInt(str(header, 100, 8).trim() || '0', 8),
      size,
      type: str(header, 156, 1),
      linkTarget: str(header, 157, 100),
      body: archive.subarray(offset + 512, offset + 512 + size),
      checksumOK: sum === stored,
    })
    offset += 512 + Math.ceil(size / 512) * 512
  }
  return entries
}

function str(buf: Buffer, start: number, length: number): string {
  const slice = buf.subarray(start, start + length)
  const end = slice.indexOf(0)
  return slice.subarray(0, end === -1 ? slice.length : end).toString('utf8')
}
