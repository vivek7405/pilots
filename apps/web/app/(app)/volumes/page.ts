/**
 * `/volumes` is now `/storage`.
 *
 * "Volume" is the engine's word for a disk that outlives the machine it is
 * attached to. "Storage" is what a person calls the same thing, and it is the
 * word every other surface in this app now uses.
 */
import { redirect } from '@webjsdev/core';

export default function VolumesRedirect(): never {
  throw redirect('/storage', 308);
}
