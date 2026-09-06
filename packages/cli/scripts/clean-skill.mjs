/**
 * postpack: remove the packed copy of the skill.
 *
 * The tarball has it; the working tree should not, or a developer ends up
 * editing whichever of the two they happened to open.
 */
import { rmSync } from 'node:fs'
import { dirname, join } from 'node:path'

rmSync(join(dirname(import.meta.dirname), 'resources'), { recursive: true, force: true })
