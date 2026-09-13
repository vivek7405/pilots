package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// The ring honours the io.Writer contract on every write, including the one
// that crosses its cap.
//
// # The bug
//
// The growth branch re-slices p to the bytes it has not consumed, and the
// returns below reported the REMAINDER as the count for the whole call. On the
// single write that crosses the cap this answered "I wrote 200" to a caller
// that passed 300, with a nil error.
//
// io.Writer forbids that: n < len(p) requires a non-nil error. The supervisor
// wires this into io.MultiWriter beside the serial console, MultiWriter turns
// the short count into io.ErrShortWrite, os/exec's copier aborts and closes the
// child's pipe, and the process is killed by EPIPE on its next write. After
// about a megabyte of output, which is to say on every real application.
func TestTheRingAcceptsEveryByteItIsHanded(t *testing.T) {
	r := newLogRing()
	chunk := bytes.Repeat([]byte("x"), 1000)

	// Well past the cap, so the crossing write and many wrapped ones happen.
	for i := range (logRingSize / len(chunk)) + 50 {
		n, err := r.Write(chunk)
		if err != nil {
			t.Fatalf("write %d returned %v", i, err)
		}
		if n != len(chunk) {
			t.Fatalf("write %d reported %d of %d bytes. A short count with a nil "+
				"error breaks io.Writer, and io.MultiWriter turns it into "+
				"ErrShortWrite, which kills the supervised process", i, n, len(chunk))
		}
	}
}

// The exact shape the supervisor builds, because that is what converts the
// contract violation into a dead application.
func TestAMultiWriterCopyRunsToCompletion(t *testing.T) {
	r := newLogRing()
	var console bytes.Buffer
	w := io.MultiWriter(r, &console)

	const total = 3 << 20 // three times the ring, so it wraps repeatedly
	n, err := io.Copy(w, strings.NewReader(strings.Repeat("y", total)))
	if err != nil {
		t.Fatalf("io.Copy stopped after %d of %d bytes: %v. os/exec does this "+
			"copy for a process's stdout, and it closes the pipe when it stops",
			n, total, err)
	}
	if n != total {
		t.Fatalf("io.Copy moved %d of %d bytes", n, total)
	}
	// The console half must have seen everything, since MultiWriter stops at
	// the first short write and never reaches the second writer.
	if console.Len() != total {
		t.Errorf("the serial console received %d of %d bytes", console.Len(), total)
	}
}

// A single write larger than the whole ring is accepted whole and keeps its
// tail, which is the part that says what happened last.
func TestAWriteBiggerThanTheRingIsStillAccepted(t *testing.T) {
	r := newLogRing()
	big := append(bytes.Repeat([]byte("a"), logRingSize), []byte("TAIL")...)

	n, err := r.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("got (%d, %v), want (%d, nil)", n, err, len(big))
	}
	if got := r.Bytes(); !bytes.HasSuffix(got, []byte("TAIL")) {
		t.Errorf("the ring kept the head rather than the tail; last bytes were %q",
			got[max(0, len(got)-8):])
	}
}

// And the ring still bounds itself, or the fix would have traded a dead
// process for an unbounded buffer.
func TestTheRingStaysBounded(t *testing.T) {
	r := newLogRing()
	for range 40 {
		if _, err := r.Write(bytes.Repeat([]byte("z"), 1<<16)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(r.Bytes()); got > logRingSize {
		t.Fatalf("the ring holds %d bytes, over its %d cap", got, logRingSize)
	}
}
