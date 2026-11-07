package terminal

import (
	"errors"
)

// ErrWriteCancelled is returned by WriteCancel when its cancel channel
// closes before every byte reached the PTY.
var ErrWriteCancelled = errors.New("terminal: PTY write cancelled")

// inputWriteChunk is the largest piece WriteCancel hands to one PTY
// write(2). A write it gives up on keeps only this copy alive, never
// the caller's whole buffer.
const inputWriteChunk = 16 << 10

// writeSlot returns the one-slot semaphore that serializes input
// writes to the PTY master. Created lazily so Terminal values built in
// tests (and by Adopt) need no extra field.
func (t *Terminal) writeSlot() chan struct{} {
	t.writeSemOnce.Do(func() { t.writeSem = make(chan struct{}, 1) })
	return t.writeSem
}

// writePTY writes p to the PTY while holding the write slot. It blocks
// for as long as the kernel write does.
func (t *Terminal) writePTY(p []byte) (int, error) {
	slot := t.writeSlot()
	slot <- struct{}{}
	defer func() { <-slot }()
	return t.ptmx.Write(p)
}

// WriteCancel writes p to the PTY like Write, but returns
// ErrWriteCancelled (with the count already written) once cancel
// closes. A nil cancel never fires.
//
// The PTY master is a blocking fd, so a write(2) already in the kernel
// cannot be interrupted: while the foreground process is stopped it
// sits there until the process reads again or the PTY closes. Each
// chunk is therefore written on its own goroutine from a private copy,
// holding the terminal's write slot. On cancel the caller returns at
// once and drops its buffer; the abandoned goroutine keeps at most one
// chunk and releases the slot when its write finishes. Because every
// writer takes the slot, a tab has at most one such stuck write no
// matter how many connections give up on it, and no later input can
// overtake bytes that are still waiting in it.
//
// onChunk, when non-nil, is called after each chunk is written, so a
// caller can track write progress.
func (t *Terminal) WriteCancel(cancel <-chan struct{}, p []byte, onChunk func(n int)) (int, error) {
	if len(p) > 0 {
		t.markInput()
	}
	slot := t.writeSlot()
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > inputWriteChunk {
			n = inputWriteChunk
		}
		select {
		case <-cancel:
			return written, ErrWriteCancelled
		case slot <- struct{}{}:
		}
		buf := append([]byte(nil), p[:n]...)
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		go func() {
			wn, err := t.ptmx.Write(buf)
			<-slot
			done <- result{wn, err}
		}()
		select {
		case <-cancel:
			return written, ErrWriteCancelled
		case r := <-done:
			written += r.n
			if r.err != nil {
				return written, r.err
			}
			if onChunk != nil {
				onChunk(r.n)
			}
		}
		p = p[n:]
	}
	return written, nil
}

// PasteBytes returns text as Paste would write it: wrapped in the
// bracketed-paste markers when the application has enabled DECSET 2004.
func (t *Terminal) PasteBytes(text string) []byte {
	if t.bracketedPaste.Load() {
		return []byte("\x1b[200~" + text + "\x1b[201~")
	}
	return []byte(text)
}
