package sshx

import (
	"bytes"
	"fmt"
	"io"

	"golang.org/x/crypto/ssh"
)

// execSession is one ssh.Session running a single remote command. The daemonless-exec
// push (PLAN §5.1 0a) execs the ABSOLUTE remote `clipbeam ingest`, streams a CB01 frame
// to its stdin, and relays the printed remote saved path (stdout).
type execSession struct {
	sess *ssh.Session
}

// Run execs remoteCommand, writes stdin fully (closing the remote stdin so the remote
// `clipbeam ingest` sees EOF and finishes its CB01 decode), and returns captured
// stdout + stderr. A non-zero remote exit is an error carrying the captured stderr so
// the caller can surface the remote diagnostic (PLAN §5.1). stdin is written from a
// pipe so a large frame is streamed, never materialized again by the ssh layer.
func (s *execSession) Run(remoteCommand string, stdin []byte) ([]byte, []byte, error) {
	var outBuf, errBuf bytes.Buffer
	s.sess.Stdout = &outBuf
	s.sess.Stderr = &errBuf

	stdinPipe, err := s.sess.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("clipbeam: ssh stdin pipe: %w", err)
	}

	if err := s.sess.Start(remoteCommand); err != nil {
		_ = stdinPipe.Close()
		return nil, nil, fmt.Errorf("clipbeam: ssh exec %q: %w", remoteCommand, err)
	}

	// Stream stdin then close it so the remote reads EOF and prints its saved path(s).
	writeErr := writeAndClose(stdinPipe, stdin)

	waitErr := s.sess.Wait()
	if writeErr != nil && waitErr == nil {
		// A stdin write error with a clean remote exit is still a transport failure.
		return outBuf.Bytes(), errBuf.Bytes(), fmt.Errorf("clipbeam: ssh write stdin: %w", writeErr)
	}
	if waitErr != nil {
		return outBuf.Bytes(), errBuf.Bytes(), fmt.Errorf("clipbeam: remote command failed: %w", waitErr)
	}
	return outBuf.Bytes(), errBuf.Bytes(), nil
}

// Close ends the session.
func (s *execSession) Close() error {
	return s.sess.Close()
}

// writeAndClose writes p to w and closes it, returning the first error. Closing the
// remote stdin is what signals EOF to `clipbeam ingest`.
func writeAndClose(w io.WriteCloser, p []byte) error {
	_, werr := w.Write(p)
	cerr := w.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
