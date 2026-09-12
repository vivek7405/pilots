#!/bin/sh
# A stand-in for the `pilot` CLI, for the end-to-end test only.
#
# `pilot exec <machine> -- sh -c '<cmd>'` runs the command on THIS machine
# rather than in a guest, so the plugin's read, write, listing and navigation
# paths can be driven with no fleet and no KVM. The "guest filesystem" is an
# ordinary temporary directory, which the test addresses by its real absolute
# path, so the guest commands need no rewriting to find it.
#
# It implements exactly the one call the plugin makes. Anything else is a
# loud failure rather than a silent pass, because a test whose stand-in
# quietly accepts a call the real CLI would reject proves nothing.
[ "$1" = "exec" ] || { echo "fake pilot: only exec is implemented, got: $*" >&2; exit 2; }
shift                     # drop `exec`
[ $# -gt 0 ] && shift     # drop the machine name
[ "$1" = "--" ] && shift  # drop the separator
[ $# -gt 0 ] || { echo "fake pilot: no command after --" >&2; exit 2; }
exec "$@"
